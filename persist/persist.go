// Package persist defines the storage contract for ygo documents and
// provides helpers that turn stored update logs back into live Docs.
//
// The package itself is storage-engine-agnostic. Reference
// implementations live under sub-packages: internal/persist/sqlite is
// the pure-Go modernc.org/sqlite backing. Implementations are
// expected to provide an append-only log of opaque V1 update bytes
// keyed by docName, plus a compaction primitive (Flush) that squashes
// the log into a single snapshot update.
//
// Backends may additionally implement VersionStore (see versions.go)
// to provide named point-in-time document versions that live
// independently of the update log: history survives Flush and
// ClearDocument, and pruning history never touches live state.
//
// The wire-format guarantee is one-way: bytes that go in via
// StoreUpdate are the bytes that come out via GetUpdates. The persist
// layer never inspects, decodes, re-encodes, or splits updates; it
// treats them as opaque blobs. This keeps the storage engine
// independent of the encoding-version evolution (V1 today, V2 later).
//
// Compaction (Flush) is the one exception: the package-level Flush
// helper does decode-and-re-encode under the hood via the encoding
// layer, but the result is still a single opaque blob from the
// storage engine's perspective.
package persist

import (
	"context"
	"errors"
	"fmt"

	"github.com/Deln0r/ygo/internal/doc"
	"github.com/Deln0r/ygo/internal/encoding"
)

// Store is the persistence contract every backing implementation
// satisfies. Implementations must be safe for concurrent use from
// multiple goroutines.
//
// Method contracts:
//
//   - StoreUpdate appends an opaque update blob to docName's log.
//     Empty blobs are rejected with ErrEmptyUpdate. The blob is
//     written atomically; on error the log is unchanged.
//
//   - GetUpdates returns every update blob stored for docName, in
//     insertion order. Returns (nil, nil) — an empty slice and no
//     error — for an unknown document.
//
//   - Flush replaces all updates for docName with a single snapshot
//     update equivalent to applying them all in order to a fresh
//     Doc and re-encoding. Idempotent: calling on a doc with zero or
//     one updates is a no-op. Flush is atomic: a reader observes
//     either the pre-flush log or the single post-flush blob, never a
//     torn state, and no data is lost. A concurrent StoreUpdate is
//     serialized against it — the writer blocks until Flush commits,
//     bounded by any backend busy timeout, past which, under sustained
//     write contention, Flush may return a transient error the caller
//     can retry.
//
//   - DocumentExists reports whether docName has any stored updates.
//
//   - ListDocuments returns the names of every document with at
//     least one stored update. Order is implementation-defined.
//
//   - ClearDocument removes every update for docName. Idempotent on
//     unknown documents.
//
//   - Close releases backing resources. Safe to call more than once;
//     calls after the first return nil.
type Store interface {
	StoreUpdate(ctx context.Context, docName string, update []byte) error
	GetUpdates(ctx context.Context, docName string) ([][]byte, error)
	Flush(ctx context.Context, docName string) error
	DocumentExists(ctx context.Context, docName string) (bool, error)
	ListDocuments(ctx context.Context) ([]string, error)
	ClearDocument(ctx context.Context, docName string) error
	Close() error
}

// ErrEmptyUpdate is returned by StoreUpdate when called with a
// zero-length update blob. A zero-length slice is the one
// unambiguous caller bug the persist layer rejects: it can never be
// a meaningful update, since the smallest valid V1 update is the
// two-byte empty update (a zero client-count varuint followed by the
// empty-delete-set terminator). The layer does not otherwise inspect
// blob structure — validating the encoding is the encoder's job — so
// that two-byte empty update is itself accepted and stored verbatim.
var ErrEmptyUpdate = errors.New("persist: update blob is empty")

// LoadDoc reconstructs a Doc by replaying every stored update for
// docName in insertion order. Returns a fresh empty Doc when the
// document has no stored updates.
//
// The returned Doc owns its own ClientID (a fresh random one unless
// overridden by opts.ClientID). Replayed updates do not affect the
// local client's clock; they integrate into the existing client lists
// the source documents produced.
func LoadDoc(ctx context.Context, s Store, docName string, opts doc.Options) (*doc.Doc, error) {
	d := doc.NewDocWithOptions(opts)
	updates, err := s.GetUpdates(ctx, docName)
	if err != nil {
		return nil, fmt.Errorf("persist.LoadDoc(%q): %w", docName, err)
	}
	if len(updates) == 0 {
		return d, nil
	}
	txn := d.WriteTxn()
	defer txn.Commit()
	for i, raw := range updates {
		upd, _, err := encoding.DecodeUpdate(raw)
		if err != nil {
			return nil, fmt.Errorf("persist.LoadDoc(%q) update[%d] decode: %w", docName, i, err)
		}
		if err := upd.Apply(txn); err != nil {
			return nil, fmt.Errorf("persist.LoadDoc(%q) update[%d] apply: %w", docName, i, err)
		}
	}
	return d, nil
}

// GetStateVector returns the V1-encoded state vector of the
// persisted state for docName. Returns nil (not empty bytes) for an
// unknown document so callers can distinguish never-seen from
// known-but-empty (the latter is impossible — an empty doc has no
// stored updates and reports DocumentExists=false).
//
// Implementation note: today this replays every update via LoadDoc to
// build the state vector. A future optimisation could maintain the SV
// in storage alongside the update log so reads avoid the replay cost;
// tracked in tech-debt.md. The current pass favours correctness over
// throughput.
func GetStateVector(ctx context.Context, s Store, docName string) ([]byte, error) {
	exists, err := s.DocumentExists(ctx, docName)
	if err != nil {
		return nil, fmt.Errorf("persist.GetStateVector(%q): %w", docName, err)
	}
	if !exists {
		return nil, nil
	}
	d, err := LoadDoc(ctx, s, docName, doc.Options{})
	if err != nil {
		return nil, err
	}
	txn := d.ReadTxn()
	defer txn.Close()
	sv := txn.Store().GetStateVector()
	return encoding.EncodeStateVector(sv, nil), nil
}

// GetDiff returns the V1 update bytes carrying the blocks in
// docName's persisted state that the remote (per remoteSV) does not
// yet have. A nil or empty remoteSV is treated as the empty state
// vector — emit everything.
//
// Returns nil for an unknown document.
//
// remoteSV is the V1-wire-encoded form (the same shape that
// EncodeStateVector produces). Decoding happens here so callers can
// pass bytes straight from a sync-protocol message.
func GetDiff(ctx context.Context, s Store, docName string, remoteSV []byte) ([]byte, error) {
	exists, err := s.DocumentExists(ctx, docName)
	if err != nil {
		return nil, fmt.Errorf("persist.GetDiff(%q): %w", docName, err)
	}
	if !exists {
		return nil, nil
	}
	d, err := LoadDoc(ctx, s, docName, doc.Options{})
	if err != nil {
		return nil, err
	}
	var sv = make(map[uint64]uint64)
	if len(remoteSV) > 0 {
		decoded, _, err := encoding.DecodeStateVector(remoteSV)
		if err != nil {
			return nil, fmt.Errorf("persist.GetDiff(%q) remoteSV decode: %w", docName, err)
		}
		sv = decoded
	}
	txn := d.ReadTxn()
	defer txn.Close()
	return encoding.EncodeDiff(d, txn, sv), nil
}

// MergeUpdates decodes every blob in updates in order, applies them
// to a fresh Doc, and returns a single V1 update blob equivalent to
// the merged state. Returns nil when updates is empty so the caller
// can treat the result as a no-op trigger.
//
// Backing implementations use this inside their Flush transaction:
// read updates, MergeUpdates them, delete the originals, insert the
// snapshot — all under one storage-level lock so concurrent writers
// see either the pre-flush log or the post-flush single blob, never
// a torn intermediate state.
//
// Error handling: on any decode or apply failure the caller MUST NOT
// proceed with the destructive replace — the original updates are
// still the canonical state. Returning the error preserves that
// contract; the caller should abort the storage transaction.
// ErrIncompleteLog reports that an update log contains updates whose causal
// dependencies are absent from the same log, so compacting it now would
// silently drop them. The log itself is intact and replays correctly (the
// pending buffer holds such updates until their dependencies arrive); retry
// the flush after more updates land. Callers that capture snapshots
// (SaveVersion) propagate it rather than record an incomplete state.
var ErrIncompleteLog = errors.New("persist: log has updates with unmet causal dependencies; compaction refused")

// MergeUpdates compacts a stored update log into a single update, for callers
// that own the log and are about to replace it.
//
// It REFUSES a log whose updates depend on causal ancestors the log does not
// contain, returning ErrIncompleteLog rather than a compacted result. That is
// deliberately different from the root ygo.MergeUpdates, which preserves such
// updates behind Skip runs: compaction here is a destructive replace, so
// refusing and retrying later is the safe answer, while coalescing for
// transport is not destructive and can carry the unresolved half forward.
func MergeUpdates(updates [][]byte) ([]byte, error) {
	if len(updates) == 0 {
		return nil, nil
	}
	d := doc.NewDoc()
	wtxn := d.WriteTxn()
	for i, raw := range updates {
		upd, _, err := encoding.DecodeUpdate(raw)
		if err != nil {
			wtxn.Commit()
			return nil, fmt.Errorf("persist.MergeUpdates update[%d] decode: %w", i, err)
		}
		if err := upd.Apply(wtxn); err != nil {
			wtxn.Commit()
			return nil, fmt.Errorf("persist.MergeUpdates update[%d] apply: %w", i, err)
		}
	}
	wtxn.Commit()
	// A dependent update whose causal ancestor is not in this log parks in
	// the reconstruction doc's pending buffer, and EncodeStateAsUpdate emits
	// only integrated state - so proceeding would DROP that update from the
	// compacted snapshot while the destructive replace erases the original
	// bytes: silent, permanent data loss. Reachable in the server: an update
	// is applied and broadcast BEFORE it is persisted, so a fast peer's
	// dependent edit can reach the Store first; a flush in that window sees
	// exactly this log. Refuse instead - the un-compacted log is
	// order-tolerant (replay re-parks and heals), so skipping compaction is
	// always safe, and the next flush after the ancestor lands succeeds.
	rtxn := d.ReadTxn()
	pending := encoding.HasPending(rtxn)
	rtxn.Close()
	if pending {
		return nil, ErrIncompleteLog
	}
	return encoding.EncodeStateAsUpdate(d), nil
}
