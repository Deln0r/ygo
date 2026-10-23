package persist

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Deln0r/ygo/internal/doc"
	"github.com/Deln0r/ygo/internal/encoding"
)

// VersionInfo is the metadata of one stored document version. The
// state blob itself is fetched separately via GetVersionState so
// listing stays cheap for documents with many large versions.
type VersionInfo struct {
	// ID is the storage-assigned identifier, monotonically increasing
	// per backend (not per document). Higher ID = newer version.
	ID int64
	// Label is the optional caller-supplied name ("before-migration",
	// "v2 draft"). Not unique; may be empty.
	Label string
	// CreatedAt is the backend-stamped creation time (UTC).
	CreatedAt time.Time
	// Size is the state blob length in bytes.
	Size int64
}

// VersionStore is the optional versioned-history extension of Store.
// A backend that implements it can capture named point-in-time
// versions of a document independent of the live update log: the live
// log can be flushed, compacted, or cleared without touching stored
// versions, and versions can be pruned without touching the live log.
//
// Method contracts:
//
//   - SaveVersion stores state (a self-contained V1 update blob
//     carrying the full document state) as a new version of docName
//     and returns its ID. Empty state is rejected with ErrEmptyUpdate.
//
//   - ListVersions returns the metadata of every version of docName,
//     oldest first. Returns (nil, nil) for a document with no
//     versions.
//
//   - GetVersionState returns the state blob of one version.
//     ErrVersionNotFound if the (docName, versionID) pair is unknown.
//
//   - DeleteVersion removes one version. Idempotent: deleting an
//     unknown version is a no-op.
//
//   - RestoreVersion replaces docName's LIVE update log with the
//     version's state blob, atomically from the perspective of
//     concurrent readers and writers. ErrVersionNotFound if unknown.
//
// VersionStore deliberately does not embed Store so the package's v1.0
// Store contract stays frozen; backends implement both, and helpers
// that need both take a VersionedStore.
type VersionStore interface {
	SaveVersion(ctx context.Context, docName, label string, state []byte) (int64, error)
	ListVersions(ctx context.Context, docName string) ([]VersionInfo, error)
	GetVersionState(ctx context.Context, docName string, versionID int64) ([]byte, error)
	DeleteVersion(ctx context.Context, docName string, versionID int64) error
	RestoreVersion(ctx context.Context, docName string, versionID int64) error
}

// VersionedStore is a backend that provides both the live update log
// and versioned history. The sqlite sub-package implements it.
type VersionedStore interface {
	Store
	VersionStore
}

// ErrVersionNotFound is returned when a (docName, versionID) pair does
// not exist.
var ErrVersionNotFound = errors.New("persist: version not found")

// ErrUnknownDocument is returned by SaveVersion when docName has no
// stored updates: an empty document has no state worth versioning.
var ErrUnknownDocument = errors.New("persist: unknown document")

// SaveVersion captures the current persisted state of docName as a new
// version labelled label and returns the version ID. The state is the
// merge of the live update log at call time; updates that land
// concurrently with the read may or may not be included (the version
// is a point-in-time capture, not a barrier).
//
// Returns ErrUnknownDocument when docName has no stored updates.
func SaveVersion(ctx context.Context, s VersionedStore, docName, label string) (int64, error) {
	updates, err := s.GetUpdates(ctx, docName)
	if err != nil {
		return 0, fmt.Errorf("persist.SaveVersion(%q): %w", docName, err)
	}
	if len(updates) == 0 {
		return 0, fmt.Errorf("persist.SaveVersion(%q): %w", docName, ErrUnknownDocument)
	}
	state, err := MergeUpdates(updates)
	if err != nil {
		return 0, fmt.Errorf("persist.SaveVersion(%q): %w", docName, err)
	}
	id, err := s.SaveVersion(ctx, docName, label, state)
	if err != nil {
		return 0, fmt.Errorf("persist.SaveVersion(%q): %w", docName, err)
	}
	return id, nil
}

// LoadVersion reconstructs the document as it was at the given
// version. The returned Doc is independent of the live log; mutating
// it does not affect stored state.
func LoadVersion(ctx context.Context, vs VersionStore, docName string, versionID int64, opts doc.Options) (*doc.Doc, error) {
	state, err := vs.GetVersionState(ctx, docName, versionID)
	if err != nil {
		return nil, fmt.Errorf("persist.LoadVersion(%q, %d): %w", docName, versionID, err)
	}
	d := doc.NewDocWithOptions(opts)
	txn := d.WriteTxn()
	txn.Local = false
	defer txn.Commit()
	upd, _, err := encoding.DecodeStoredUpdate(state)
	if err != nil {
		return nil, fmt.Errorf("persist.LoadVersion(%q, %d) decode: %w", docName, versionID, err)
	}
	if err := upd.Apply(txn); err != nil {
		return nil, fmt.Errorf("persist.LoadVersion(%q, %d) apply: %w", docName, versionID, err)
	}
	return d, nil
}

// PruneVersions deletes all but the newest keep versions of docName
// and returns how many were removed. keep <= 0 removes every version.
// Idempotent: a crash mid-prune leaves extra versions behind, and a
// re-run removes them; the live log is never touched.
func PruneVersions(ctx context.Context, vs VersionStore, docName string, keep int) (int, error) {
	infos, err := vs.ListVersions(ctx, docName)
	if err != nil {
		return 0, fmt.Errorf("persist.PruneVersions(%q): %w", docName, err)
	}
	if keep < 0 {
		keep = 0
	}
	if len(infos) <= keep {
		return 0, nil
	}
	// ListVersions is oldest-first; everything before the last keep
	// entries goes.
	deleted := 0
	for _, info := range infos[:len(infos)-keep] {
		if err := vs.DeleteVersion(ctx, docName, info.ID); err != nil {
			return deleted, fmt.Errorf("persist.PruneVersions(%q) delete %d: %w", docName, info.ID, err)
		}
		deleted++
	}
	return deleted, nil
}
