package ygo

// Public API surface for the ygo CRDT framework.
//
// External Go code imports `github.com/Deln0r/ygo` and uses the
// types and functions re-exported here. The implementation lives
// under internal/* — the aliases below pin those internal types
// to a stable name so callers do not need to know the package
// layout. For sub-systems with their own concerns (persistence,
// WebSocket server) see:
//
//   - github.com/Deln0r/ygo/persist        (Store interface)
//   - github.com/Deln0r/ygo/persist/sqlite (pure-Go SQLite impl)
//   - github.com/Deln0r/ygo/server         (WebSocket sync server)
//
// Type aliases (vs wrapper structs) keep method sets intact at
// zero runtime cost — `*ygo.Map` is exactly `*types.Map`, so
// SetMap / SetArray / SetText return the right type without any
// wrapping or copying.

import (
	"errors"
	"fmt"

	"github.com/Deln0r/ygo/internal/awareness"
	"github.com/Deln0r/ygo/internal/block"
	"github.com/Deln0r/ygo/internal/doc"
	"github.com/Deln0r/ygo/internal/encoding"
	"github.com/Deln0r/ygo/internal/store"
	"github.com/Deln0r/ygo/internal/types"
	"github.com/Deln0r/ygo/internal/undo"
)

// Doc is a single CRDT replica — the local view of a collaborative
// document. Construct with NewDoc; mutate via WriteTxn; read via
// ReadTxn. See the doc-comment on the underlying type for the full
// concurrency contract.
type Doc = doc.Doc

// Options bundles per-Doc settings (deterministic ClientID, GC
// disable). The zero value is the recommended configuration.
type Options = doc.Options

// Transaction is a read-only transaction holding the doc's read
// lock for its lifetime. Created by Doc.ReadTxn; released by Close.
type Transaction = doc.Transaction

// TransactionMut is a write transaction holding the doc's write
// lock. Created by Doc.WriteTxn; released by Commit.
type TransactionMut = doc.TransactionMut

// NewDoc returns a fresh Doc with default options and a random
// client identifier.
func NewDoc() *Doc { return doc.NewDoc() }

// NewDocWithOptions returns a fresh Doc with the given options.
func NewDocWithOptions(opts Options) *Doc { return doc.NewDocWithOptions(opts) }

// Branch is the low-level shared-data container the types layer
// wraps. Most callers should use the typed constructors (NewMap,
// NewArray, NewText, NewXmlFragment) rather than building Branches
// directly. Re-exported here for advanced cases (custom shared-type
// implementations, observability code).
type Branch = block.Branch

// MaxClientID is the upper bound on Doc.ClientID values
// (2^53 - 1, matching JS Yjs's safe-integer range).
const MaxClientID = doc.MaxClientID

// Shared-type wrappers — re-exported from internal/types.
type (
	Map         = types.Map
	Array       = types.Array
	Text        = types.Text
	XmlFragment = types.XmlFragment
	XmlElement  = types.XmlElement
	XmlText     = types.XmlText

	// Attrs is the format-attribute map used by Text's rich-text
	// API and DeltaOp. Keys are arbitrary strings; values are
	// JSON-serializable scalars. A nil value clears the attribute
	// on the affected range.
	Attrs = types.Attrs

	// DeltaOp is one Quill-style delta operation produced by
	// Text.ToDelta and (future) consumed by Text.ApplyDelta.
	DeltaOp = types.DeltaOp

	// ChunkKind discriminates the variants emitted by Text.Range.
	ChunkKind = types.ChunkKind
)

// Text.Range emits chunks of either of these kinds.
const (
	ChunkString = types.ChunkString
	ChunkEmbed  = types.ChunkEmbed
)

// NewMap, NewArray, NewText, NewXmlFragment return wrappers bound
// to the root branch with the given name in d. The branch is
// lazily created on first call; subsequent calls with the same
// name return wrappers pointing at the same underlying state.
//
// Per-branch type discipline: a branch should be used as ONE type
// (Map OR Array OR Text OR XML). Mixing types on the same root
// branch produces undefined behaviour.
func NewMap(d *Doc, name string) *Map {
	return types.NewMap(d.Branch(name))
}

func NewArray(d *Doc, name string) *Array {
	return types.NewArray(d.Branch(name))
}

func NewText(d *Doc, name string) *Text {
	return types.NewText(d.Branch(name))
}

func NewXmlFragment(d *Doc, name string) *XmlFragment {
	return types.NewXmlFragment(d.Branch(name))
}

// NewXmlElement wraps a branch as an XmlElement. Typically used
// for root-level XML where the branch was constructed via
// d.Branch(name); nested elements should be constructed via
// XmlFragment.InsertXmlElement / XmlElement.InsertXmlElement which
// set TypeRef and Name automatically.
func NewXmlElement(d *Doc, name string) *XmlElement {
	return types.NewXmlElement(d.Branch(name))
}

func NewXmlText(d *Doc, name string) *XmlText {
	return types.NewXmlText(d.Branch(name))
}

// UndoManager records local mutations under a scope of shared types
// and reverses them with Undo / Redo. It subscribes to the doc's
// transaction lifecycle; close it with Close when no longer needed.
//
// Scope is given as the typed wrappers themselves (a Map, Array, Text,
// or XML type). Only mutations under one of the scoped types, made by
// a tracked origin (local edits by default), are captured. Bursty edits
// within the capture-timeout window collapse into a single undo step.
//
//	m := ygo.NewMap(d, "settings")
//	um := ygo.NewUndoManager(d, m)
//	defer um.Close()
//	// ... edits to m ...
//	um.Undo() // reverts the last captured step
type UndoManager = undo.UndoManager

// UndoManagerOptions configures an UndoManager. A zero value selects
// the defaults: 500 ms capture timeout, track local (nil) origin only.
type UndoManagerOptions = undo.Options

// SubdocsEvent carries the subdocument lifecycle changes of one
// transaction: GUIDs added, removed, and loaded. Observe with
// Doc.OnSubdocs.
type SubdocsEvent = doc.SubdocsEvent

// MapEvent is delivered to Map observers (Map.Observe) after a
// transaction that changed the map, describing which keys changed and
// how. Byte-for-byte semantic parity with yjs YMapEvent.
type MapEvent = types.MapEvent

// KeyChange describes one map key's change in a MapEvent: Action is
// "add", "update", or "delete"; OldValue is the prior value (nil for
// "add").
type KeyChange = types.KeyChange

// ArrayEvent is delivered to Array observers (Array.Observe) after a
// transaction that changed the array. Delta is the Quill-style change
// description. Semantic parity with yjs YArrayEvent.
type ArrayEvent = types.ArrayEvent

// ArrayDeltaOp is one op of an array change delta: exactly one of
// Insert (values), Delete (count), or Retain (count) is set.
type ArrayDeltaOp = types.ArrayDeltaOp

// TextEvent is delivered to Text observers (Text.Observe) after a
// transaction that changed the text. Delta is the formatting-aware
// Quill-style change description. Semantic parity with yjs YTextEvent.
type TextEvent = types.TextEvent

// UndoScope is anything an UndoManager can watch. Every shared-type
// wrapper (Map, Array, Text, XmlFragment, XmlElement, XmlText)
// satisfies it via its Branch method.
type UndoScope interface {
	Branch() *Branch
}

// NewUndoManager creates an UndoManager on d watching the given scope
// of shared types. At least one scope type is required.
//
//	um := ygo.NewUndoManager(d, myMap, myArray)
func NewUndoManager(d *Doc, scope ...UndoScope) *UndoManager {
	return newUndoManager(d, UndoManagerOptions{}, scope)
}

// NewUndoManagerWithOptions is NewUndoManager with explicit options.
func NewUndoManagerWithOptions(d *Doc, opts UndoManagerOptions, scope ...UndoScope) *UndoManager {
	return newUndoManager(d, opts, scope)
}

func newUndoManager(d *Doc, opts UndoManagerOptions, scope []UndoScope) *UndoManager {
	branches := make([]*block.Branch, len(scope))
	for i, s := range scope {
		branches[i] = s.Branch()
	}
	return undo.NewUndoManager(d, branches, opts)
}

// RelativePosition is a cursor/selection anchor attached to the
// document model rather than a numeric index, so it stays on the same
// logical character as concurrent edits land. Create one with
// CreateRelativePositionFromTypeIndex, ship it between peers with
// EncodeRelativePosition (byte-compatible with Y.encodeRelativePosition),
// and resolve it back to an index with
// CreateAbsolutePositionFromRelativePosition.
type RelativePosition = types.RelativePosition

// AbsolutePosition is a resolved RelativePosition: the shared type's
// branch and the current numeric index within it.
type AbsolutePosition = types.AbsolutePosition

// SharedType is any shared-type wrapper (Map, Array, Text,
// XmlFragment, XmlElement, XmlText). Alias of UndoScope; both name the
// same one-method interface.
type SharedType = UndoScope

// CreateRelativePositionFromTypeIndex anchors index within the shared
// type t (any wrapper: Map, Array, Text, Xml*). index counts UTF-16
// code units for Text, elements for Array. assoc >= 0 sticks to the
// character after the position (default for cursors), assoc < 0 to the
// character before it.
func CreateRelativePositionFromTypeIndex(t SharedType, index uint64, assoc int64) (RelativePosition, error) {
	return types.CreateRelativePositionFromTypeIndex(t, index, assoc)
}

// EncodeRelativePosition serialises rpos to the yjs binary form
// (byte-compatible with Y.encodeRelativePosition). Errors only on a
// zero-value rpos with no anchor set.
func EncodeRelativePosition(rpos RelativePosition) ([]byte, error) {
	return types.EncodeRelativePosition(rpos)
}

// DecodeRelativePosition parses the yjs binary form.
func DecodeRelativePosition(buf []byte) (RelativePosition, error) {
	return types.DecodeRelativePosition(buf)
}

// CreateAbsolutePositionFromRelativePosition resolves rpos against the
// current state of d. ok is false when the anchor refers to state this
// replica has not yet seen or to a garbage-collected range.
//
// Acquires the doc's locks internally; calling it while holding an
// open Transaction / TransactionMut on the same doc from the same
// goroutine deadlocks (Go RWMutex is not re-entrant).
func CreateAbsolutePositionFromRelativePosition(d *Doc, rpos RelativePosition) (AbsolutePosition, bool) {
	return types.CreateAbsolutePositionFromRelativePosition(d, rpos)
}

// Awareness tracks per-client ephemeral state (cursors, names,
// selections). Independent of any Doc; the local clientID is
// passed at construction. Embedders typically pair an Awareness
// with a Doc and use d.ClientID() as the awareness clientID.
type Awareness = awareness.Awareness

// NewAwareness returns a fresh Awareness for the given local
// clientID. Use d.ClientID() to keep the awareness layer in sync
// with the doc.
func NewAwareness(clientID uint64) *Awareness { return awareness.New(clientID) }

// DefaultAwarenessTimeout is the y-protocols convention for stale
// awareness-entry eviction (30 seconds). Pass to
// Awareness.SweepOutdated.
const DefaultAwarenessTimeout = awareness.DefaultTimeout

// EncodeStateAsUpdate returns the wire-encoded V1 update carrying
// the doc's full state. Apply to a fresh peer doc to bring it up
// to speed in one shot; same bytes interoperate with JS Yjs's
// Y.encodeStateAsUpdate.
func EncodeStateAsUpdate(d *Doc) []byte {
	return encoding.EncodeStateAsUpdate(d)
}

// EncodeStateAsUpdateV2 returns the wire-encoded V2 update carrying
// the doc's full state. V2 is the column-oriented alternative wire
// format used by Y.encodeStateAsUpdateV2 / Hocuspocus-V2 paths and
// some adopters' on-disk persistence layers (y-leveldb, y-indexeddb,
// SQLite/Postgres Hocuspocus adapters).
//
// V1 and V2 are NOT wire-interchangeable — see ApplyUpdateV2.
func EncodeStateAsUpdateV2(d *Doc) []byte {
	return encoding.EncodeStateAsUpdateV2(d)
}

// EncodeStateVector returns the wire-encoded V1 state vector of d.
// Sync-protocol callers send this to peers as "here's what I have;
// send me everything else."
func EncodeStateVector(d *Doc) []byte {
	t := d.ReadTxn()
	defer t.Close()
	return encoding.EncodeStateVector(t.Store().GetStateVector(), nil)
}

// Snapshot is a point-in-time marker of a document's history (the
// deleted ID ranges plus per-client clock heads as of the snapshot).
// Encode it with EncodeSnapshot for storage; reconstruct the document
// state it captured with RestoreSnapshot (see the Snapshot doc).
//
// For time-travel to work, create the doc with GC disabled
// (ygo.NewDocWithOptions with DisableGC) so deleted content the
// snapshot references is retained.
type Snapshot = encoding.Snapshot

// CreateSnapshot captures the current state of d as a Snapshot.
// Byte-compatible with yjs `Y.snapshot(doc)`.
func CreateSnapshot(d *Doc) Snapshot {
	t := d.ReadTxn()
	defer t.Close()
	return encoding.CreateSnapshot(t.Store())
}

// EncodeSnapshot returns the V1 wire encoding of s, byte-compatible
// with yjs `Y.encodeSnapshot`.
func EncodeSnapshot(s Snapshot) []byte { return encoding.EncodeSnapshot(s) }

// DecodeSnapshot parses a V1 snapshot produced by EncodeSnapshot or
// yjs `Y.encodeSnapshot`.
func DecodeSnapshot(buf []byte) (Snapshot, error) { return encoding.DecodeSnapshot(buf) }

// EqualSnapshots reports whether two snapshots are identical.
func EqualSnapshots(a, b Snapshot) bool { return encoding.EqualSnapshots(a, b) }

// RestoreSnapshot reconstructs the document state that d had at the
// moment snap was taken, returning it as a new Doc. Byte-equivalent to
// yjs `Y.createDocFromSnapshot`.
//
// d must have been created with GC disabled (ygo.NewDocWithOptions with
// DisableGC: true); otherwise deleted content the snapshot references
// may have been collected and RestoreSnapshot returns ErrSnapshotGC.
// The returned Doc also has GC disabled so it can itself be snapshotted.
//
// The source Doc is not logically changed (the reconstruction may split
// blocks internally, which is transparent to readers).
func RestoreSnapshot(d *Doc, snap Snapshot) (*Doc, error) {
	if d.GC() {
		return nil, ErrSnapshotGC
	}
	txn := d.WriteTxn()
	encoding.SplitAtSnapshotBoundaries(txn.Store(), snap)
	update := encoding.EncodeSnapshotAsUpdateV1(txn.Store(), snap)
	txn.Commit()

	nd := NewDocWithOptions(Options{DisableGC: true})
	if err := ApplyUpdate(nd, update); err != nil {
		return nil, err
	}
	return nd, nil
}

// ErrSnapshotGC is returned by RestoreSnapshot when the source Doc has
// garbage collection enabled, which can discard content a snapshot
// needs to reconstruct.
var ErrSnapshotGC = errors.New("ygo: RestoreSnapshot requires the source Doc to have GC disabled (NewDocWithOptions with DisableGC: true)")

// EncodeDiff returns the wire-encoded V1 update covering the
// blocks d has that the remote (per remoteSVBytes) does not. A
// nil remoteSVBytes is treated as the empty SV — emit everything.
//
// remoteSVBytes is the V1 wire-encoded form of the remote's state
// vector (the same shape EncodeStateVector produces).
func EncodeDiff(d *Doc, remoteSVBytes []byte) ([]byte, error) {
	var sv store.StateVector
	if len(remoteSVBytes) > 0 {
		decoded, _, err := encoding.DecodeStateVector(remoteSVBytes)
		if err != nil {
			return nil, err
		}
		sv = decoded
	}
	t := d.ReadTxn()
	defer t.Close()
	return encoding.EncodeDiff(d, t, sv), nil
}

// EncodeDiffV2 is the V2 analogue of EncodeDiff. State-vector
// argument shape is identical (still V1 wire-encoded SV); only the
// outgoing update bytes use the V2 column layout.
func EncodeDiffV2(d *Doc, remoteSVBytes []byte) ([]byte, error) {
	var sv store.StateVector
	if len(remoteSVBytes) > 0 {
		decoded, _, err := encoding.DecodeStateVector(remoteSVBytes)
		if err != nil {
			return nil, err
		}
		sv = decoded
	}
	t := d.ReadTxn()
	defer t.Close()
	return encoding.EncodeDiffV2(d, t, sv), nil
}

// ApplyUpdate decodes raw and integrates it into d. Items whose
// dependencies the local store has not yet seen - an unseen Origin,
// RightOrigin or Parent, or a gap in the item's own client clock -
// queue in the per-doc pending buffer and drain automatically on
// subsequent ApplyUpdate calls that satisfy them.
//
// It decodes ONE update and ignores any bytes after it, so a buffer
// holding two appended updates applies only the first and loses the
// second with no error (yjs behaves identically). Run ValidateUpdate
// first on anything you did not encode yourself.
//
// Use HasPending / MissingSV to inspect the queue.
func ApplyUpdate(d *Doc, raw []byte) error {
	return encoding.ApplyUpdate(d, raw)
}

// ApplyUpdateV2 decodes V2 wire bytes and integrates them into d.
// Pending-buffer semantics identical to ApplyUpdate (V1) — items
// missing causal dependencies queue silently and drain on
// subsequent ApplyUpdate / ApplyUpdateV2 calls.
//
// Same trailing-byte behaviour as ApplyUpdate: one update is read
// and the rest of the buffer is ignored. There is no strict
// validator for V2 bytes, because the V2 decoder does not report a
// remainder - so for V2 the check has to be "I encoded this myself".
//
// V1 and V2 are NOT wire-interchangeable, and the two mistakes fail
// differently. ApplyUpdateV2 on V1 bytes errors loudly. ApplyUpdate
// on V2 bytes does NOT: the V1 decoder reads the V2 prefix as a
// complete, empty update and returns nil, so the call is a silent
// no-op (measured). There is no autodetect; the caller must know
// which version they have via the surrounding transport metadata.
func ApplyUpdateV2(d *Doc, raw []byte) error {
	return encoding.ApplyUpdateV2(d, raw)
}

// ValidateUpdate returns an error unless raw is a well-formed V1 update that
// is consumed in its entirety, WITHOUT integrating it into any document. Use
// it on updates that arrive from somewhere you do not control.
//
// It is stricter than ApplyUpdate in the one way that matters for untrusted
// input: ApplyUpdate decodes a single update and silently ignores whatever
// bytes follow it. So `append(a, b...)` applies only a, loses b, and reports
// no error anywhere - the reference implementation (yjs 13.6.32, measured)
// behaves the same, which is why ApplyUpdate keeps that behaviour and this
// separate check exists. Combine updates with MergeUpdates, never with
// append. Note the boundary: MergeUpdates returns nil for an empty batch, and
// nil is not a valid update, so guard the length before validating.
//
// Validating costs a decode, not an integrate: it does not build a document
// or run conflict resolution. It does allocate in proportion to the input, so
// a network-facing caller should bound the payload size before calling rather
// than treating this as a free filter on unbounded bytes.
//
// V1 only. The V2 decoder does not report a trailing remainder, so there is
// no equivalent strict check for V2 bytes; passing V2 bytes here is reported
// as such rather than misdiagnosed.
func ValidateUpdate(raw []byte) error {
	upd, rest, err := encoding.DecodeUpdate(raw)
	if err != nil {
		return fmt.Errorf("ygo: invalid update: %w", err)
	}
	if len(rest) == 0 {
		return nil
	}
	// A V2 update starts with bytes a V1 decoder happily reads as a complete
	// but EMPTY update, so without this the caller is told their V2 bytes are
	// "concatenated updates" and pointed at MergeUpdates, which cannot help.
	if len(upd.Blocks) == 0 {
		if _, v2err := encoding.DecodeUpdateV2(raw); v2err == nil {
			return fmt.Errorf("ygo: invalid update: these are V2 wire bytes, not V1 (the V1 decoder read them as an empty update with %d trailing byte(s)); use ApplyUpdateV2, and note there is no strict validator for V2", len(rest))
		}
	}
	return fmt.Errorf("ygo: invalid update: %d trailing byte(s) after one complete update (concatenated updates are not a valid update - use MergeUpdates)", len(rest))
}

// HasPending reports whether d has any queued items awaiting
// causal dependencies.
func HasPending(d *Doc) bool {
	t := d.ReadTxn()
	defer t.Close()
	return encoding.HasPending(t)
}

// MissingSV returns the wire-encoded V1 state vector identifying
// the clocks d needs to receive in order to drain its pending
// buffer. An empty result means the queue is empty.
//
// Sync-protocol callers send this to peers as a re-fetch request:
// "I am stuck on items that need updates past this clock."
func MissingSV(d *Doc) []byte {
	t := d.ReadTxn()
	defer t.Close()
	sv := encoding.MissingSV(t)
	return encoding.EncodeStateVector(sv, nil)
}

// MergeUpdates decodes every blob in updates in order, applies
// them to a fresh Doc, and returns a single V1 update blob
// equivalent to the merged state. Returns nil for an empty input.
//
// Used by persistence layers for compaction (Flush) and by
// transports that want to batch-coalesce updates before sending.
func MergeUpdates(updates [][]byte) ([]byte, error) {
	// Use the persist package's helper directly — the function is
	// stateless and re-exporting it would create a dependency cycle
	// between root ygo and persist. Inline the small wrapper here.
	if len(updates) == 0 {
		return nil, nil
	}
	d := doc.NewDoc()
	wtxn := d.WriteTxn()
	for _, raw := range updates {
		upd, _, err := encoding.DecodeUpdate(raw)
		if err != nil {
			wtxn.Commit()
			return nil, err
		}
		if err := upd.Apply(wtxn); err != nil {
			wtxn.Commit()
			return nil, err
		}
	}
	wtxn.Commit()
	return encoding.EncodeStateAsUpdate(d), nil
}

// MergeUpdatesV2 is the V2 counterpart of MergeUpdates: it coalesces V2
// update blobs into a single equivalent V2 update. Returns nil for an
// empty input. Like MergeUpdates it reconstructs then re-encodes, so
// blocks whose causal dependencies are absent across the merged set are
// dropped rather than preserved behind Skip blocks (as yjs
// mergeUpdatesV2 would); ygo-produced full updates are self-contained and
// unaffected.
func MergeUpdatesV2(updates [][]byte) ([]byte, error) {
	if len(updates) == 0 {
		return nil, nil
	}
	d := doc.NewDoc()
	wtxn := d.WriteTxn()
	for _, raw := range updates {
		upd, err := encoding.DecodeUpdateV2(raw)
		if err != nil {
			wtxn.Commit()
			return nil, err
		}
		if err := upd.Apply(wtxn); err != nil {
			wtxn.Commit()
			return nil, err
		}
	}
	wtxn.Commit()
	return encoding.EncodeStateAsUpdateV2(d), nil
}

// EncodeStateVectorFromUpdate computes the state vector a V1 update
// advances to, directly from the update bytes without reconstructing a
// document. Mirrors yjs encodeStateVectorFromUpdate: index or diff stored
// updates server-side without loading the full document into memory.
func EncodeStateVectorFromUpdate(update []byte) ([]byte, error) {
	return encoding.EncodeStateVectorFromUpdate(update)
}

// DiffUpdate returns the part of a V1 update that a peer identified by
// remoteSV (a wire-encoded state vector, e.g. from EncodeStateVector or
// EncodeStateVectorFromUpdate) is missing. Mirrors yjs diffUpdate: trim a
// stored update before sending it to a peer that already has part of it.
//
// It reconstructs the update's state and diffs against remoteSV, emitting
// whole blocks like EncodeDiff. A full update that ygo produces
// (EncodeStateAsUpdate / MergeUpdates) is self-contained and fully
// covered; a diff (from EncodeDiff) or a hand-crafted update whose block
// dependencies are absent drops those unresolved blocks.
func DiffUpdate(update, remoteSV []byte) ([]byte, error) {
	d := NewDoc()
	if err := ApplyUpdate(d, update); err != nil {
		return nil, err
	}
	return EncodeDiff(d, remoteSV)
}
