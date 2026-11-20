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

	"github.com/Deln0r/ygo/internal/anyval"
	"github.com/Deln0r/ygo/internal/awareness"
	"github.com/Deln0r/ygo/internal/block"
	"github.com/Deln0r/ygo/internal/doc"
	"github.com/Deln0r/ygo/internal/encoding"
	"github.com/Deln0r/ygo/internal/lib0"
	"github.com/Deln0r/ygo/internal/store"
	"github.com/Deln0r/ygo/internal/types"
	"github.com/Deln0r/ygo/internal/undo"
	"github.com/Deln0r/ygo/internal/utf16"
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

// Undefined is a JavaScript undefined stored as a map value or inside an Any
// array or object. It is not nil, which is null: Map.Get returns Undefined{}
// for a key yjs set to undefined, and Map.Set(txn, k, Undefined{}) stores
// one. It encodes as lib0's undefined, so JavaScript peers read undefined.
type Undefined = block.Undefined

// BigInt is a JavaScript BigInt, carried by lib0 as a signed 64-bit integer.
// It is not an int64, which is a JavaScript number: a BigInt a peer stored
// reads as BigInt and is written back as one, and a BigInt set from Go
// reaches JavaScript as a BigInt. In a V1 format attribute or embed, which
// is JSON, it is written as a number and every reader gets a float64.
type BigInt = block.BigInt

// MaxClientID is the upper bound on Doc.ClientID values (2^53 - 1,
// matching JS Yjs's safe-integer range): every ID on the wire fits.
// Generated IDs are 32-bit, as yjs draws them.
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
//
// These take the document's write lock to find and type the root, so
// another goroutine's transaction never sees a root half typed. Inside
// an open transaction on the same document they would block forever:
// use MapIn, ArrayIn, TextIn and the other *In forms there, as yjs
// allows getMap inside transact.
func NewMap(d *Doc, name string) *Map {
	var m *Map
	d.WithRoot(utf16.ValidString(name), func(b *Branch) { m = types.NewMap(b) })
	return m
}

func NewArray(d *Doc, name string) *Array {
	var a *Array
	d.WithRoot(utf16.ValidString(name), func(b *Branch) { a = types.NewArray(b) })
	return a
}

func NewText(d *Doc, name string) *Text {
	var t *Text
	d.WithRoot(utf16.ValidString(name), func(b *Branch) { t = types.NewText(b) })
	return t
}

func NewXmlFragment(d *Doc, name string) *XmlFragment {
	var f *XmlFragment
	d.WithRoot(utf16.ValidString(name), func(b *Branch) { f = types.NewXmlFragment(b) })
	return f
}

// NewXmlElement wraps a branch as an XmlElement. Typically used
// for root-level XML where the branch was constructed via
// d.Branch(name); nested elements should be constructed via
// XmlFragment.InsertXmlElement / XmlElement.InsertXmlElement which
// set TypeRef and Name automatically.
func NewXmlElement(d *Doc, name string) *XmlElement {
	var e *XmlElement
	d.WithRoot(utf16.ValidString(name), func(b *Branch) { e = types.NewXmlElement(b) })
	return e
}

func NewXmlText(d *Doc, name string) *XmlText {
	var x *XmlText
	d.WithRoot(utf16.ValidString(name), func(b *Branch) { x = types.NewXmlText(b) })
	return x
}

// MapIn, ArrayIn, TextIn, XmlFragmentIn, XmlElementIn and XmlTextIn are
// NewMap and the others for use inside an open transaction: they find
// or create the root through txn, under the lock txn already holds, as
// yjs getMap does inside transact (yrs get_or_insert_map on a
// transaction). The type they return is the same as the document forms
// return for the same name.
func MapIn(txn *TransactionMut, name string) *Map {
	return types.NewMap(txn.GetOrCreateBranch(utf16.ValidString(name)))
}

func ArrayIn(txn *TransactionMut, name string) *Array {
	return types.NewArray(txn.GetOrCreateBranch(utf16.ValidString(name)))
}

func TextIn(txn *TransactionMut, name string) *Text {
	return types.NewText(txn.GetOrCreateBranch(utf16.ValidString(name)))
}

func XmlFragmentIn(txn *TransactionMut, name string) *XmlFragment {
	return types.NewXmlFragment(txn.GetOrCreateBranch(utf16.ValidString(name)))
}

func XmlElementIn(txn *TransactionMut, name string) *XmlElement {
	return types.NewXmlElement(txn.GetOrCreateBranch(utf16.ValidString(name)))
}

func XmlTextIn(txn *TransactionMut, name string) *XmlText {
	return types.NewXmlText(txn.GetOrCreateBranch(utf16.ValidString(name)))
}

// UndoManager records local mutations under a scope of shared types
// and reverses them with Undo / Redo. It subscribes to the doc's
// transaction lifecycle; close it with Close when no longer needed.
//
// Scope is given as the typed wrappers themselves (a Map, Array, Text,
// or XML type). Only mutations under one of the scoped types, made by
// a tracked origin, are captured. The default tracks the nil origin:
// local edits made without one, but also updates applied with plain
// ApplyUpdate, exactly as yjs does. Apply a peer's updates with
// ApplyUpdateWithOrigin to keep them out of the history, or track only
// your own origin (AddTrackedOrigin, RemoveTrackedOrigin(nil)). Bursty
// edits within the capture-timeout window collapse into a single undo
// step.
//
//	m := ygo.NewMap(d, "settings")
//	um := ygo.NewUndoManager(d, m)
//	defer um.Close()
//	// ... edits to m ...
//	um.Undo() // reverts the last captured step
type UndoManager = undo.UndoManager

// UndoManagerOptions configures an UndoManager. A zero value selects
// the defaults: 500 ms capture timeout, track the nil origin only.
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

// AwarenessSummary is what an Awareness.OnChange callback is told: the
// clients whose state was added, updated or removed by one change.
type AwarenessSummary = awareness.Summary

// NewAwareness returns a fresh Awareness for the given local
// clientID, typically d.ClientID(). The awareness keeps that ID when
// the doc later takes a new one (Doc.ClientID), as y-protocols does.
func NewAwareness(clientID uint64) *Awareness { return awareness.New(clientID) }

// DefaultAwarenessTimeout is the y-protocols convention for stale
// awareness-entry eviction (30 seconds). Pass to
// Awareness.SweepOutdated.
const DefaultAwarenessTimeout = awareness.DefaultTimeout

// EncodeStateAsUpdate returns the wire-encoded V1 update carrying
// the doc's full state. Apply to a fresh peer doc to bring it up
// to speed in one shot; same bytes interoperate with JS Yjs's
// Y.encodeStateAsUpdate. As there, the state includes updates d holds
// but cannot integrate yet (its pending buffer, see HasPending), with
// Skip runs over the clocks nobody has sent, so a peer brought up to
// speed from d gets them too; until what they wait for arrives, they
// wait in that peer's pending buffer.
func EncodeStateAsUpdate(d *Doc) []byte {
	return encoding.EncodeStateAsUpdateWithPending(d)
}

// EncodeStateAsUpdateV2 returns the wire-encoded V2 update carrying
// the doc's full state. V2 is the column-oriented alternative wire
// format used by Y.encodeStateAsUpdateV2 / Hocuspocus-V2 paths and
// some adopters' on-disk persistence layers (y-leveldb, y-indexeddb,
// SQLite/Postgres Hocuspocus adapters).
//
// V1 and V2 are NOT wire-interchangeable — see ApplyUpdateV2.
func EncodeStateAsUpdateV2(d *Doc) []byte {
	return encoding.EncodeStateAsUpdateV2WithPending(d)
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
// yjs `Y.createDocFromSnapshot(doc, snap, new Y.Doc({gc: false}))`: the
// restore goes through V2, as yjs does, and the returned Doc keeps the
// deleted content the snapshot holds (the default Y.Doc of
// createDocFromSnapshot collects it).
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
	// V2, as yjs createDocFromSnapshot writes it.
	upd, err := encoding.DecodeOwnUpdateV2(encoding.EncodeSnapshotAsUpdateV2(txn.Store(), snap))
	var updateV1 []byte
	if err != nil {
		// d may hold strings that a release before v1.26 stored with
		// invalid UTF-8. V2 writes all strings into one column, so they
		// come back as an invalid column or, joined, as valid UTF-8 cut
		// inside a character (ErrSplitSurrogate). V1 reads each string on
		// its own: when a strict V1 read refuses one, rebuild from V1, read
		// as stored (V1 cannot carry an undefined format value or embed).
		// Any other error is returned.
		v1 := encoding.EncodeSnapshotAsUpdateV1(txn.Store(), snap)
		if _, _, verr := encoding.DecodeUpdate(v1); errors.Is(verr, lib0.ErrInvalidUTF8) {
			updateV1, err = v1, nil
		}
	}
	txn.Commit()
	if err != nil {
		return nil, err
	}

	nd := NewDocWithOptions(Options{DisableGC: true})
	if updateV1 != nil {
		if err := encoding.ApplyStoredUpdate(nd, updateV1); err != nil {
			return nil, err
		}
		return nd, nil
	}
	w := nd.WriteTxn()
	w.Local = false
	err = upd.Apply(w)
	w.Commit()
	if err != nil {
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
// Each client's first block is cut at the remote's clock, so the
// bytes are those of Y.encodeStateAsUpdate(doc, sv): the updates d's
// pending buffer holds are appended from sv on, as yjs appends them,
// and the delete set is the whole document's with the pending deletes.
// The store and the waiting updates are merged as yjs merges them, so
// where waiting updates cover the same clocks, the one yjs keeps is kept.
// For one transaction's changes alone, use Doc.OnUpdate, which never
// carries the pending buffer.
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
	return encoding.EncodeDiffWithPending(d, t, sv), nil
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
	return encoding.EncodeDiffV2WithPending(d, t, sv), nil
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

// ApplyUpdateWithOrigin is ApplyUpdate with origin as the transaction's
// Origin, the third argument of yjs Y.applyUpdate. Observers, OnUpdate
// handlers and UndoManager origin filters see it, which is how a sync
// provider keeps from sending a peer's update back out:
//
//	d.OnUpdate(func(update []byte, origin any) {
//		if origin != peer {
//			broadcast(update)
//		}
//	})
//	err := ygo.ApplyUpdateWithOrigin(d, fromPeer, peer)
//
// origin must be a comparable value (a string, a pointer, a struct of
// comparable fields) if an UndoManager tracks origins by it.
func ApplyUpdateWithOrigin(d *Doc, raw []byte, origin any) error {
	return encoding.ApplyUpdateWithOrigin(d, raw, origin)
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

// ApplyUpdateV2WithOrigin is ApplyUpdateV2 with origin as the
// transaction's Origin; see ApplyUpdateWithOrigin.
func ApplyUpdateV2WithOrigin(d *Doc, raw []byte, origin any) error {
	return encoding.ApplyUpdateV2WithOrigin(d, raw, origin)
}

// ErrInvalidUTF8 is wrapped by the errors ApplyUpdate, ApplyUpdateV2,
// ValidateUpdate, MergeUpdates, MergeUpdatesV2, Awareness.Apply and
// DecodeRelativePosition return when a string in the bytes is not valid
// UTF-8. lib0 reads every string with a fatal UTF-8 decoder, so yjs rejects
// the same bytes; accepting them would let one update keep every JS client
// of a document from loading it. Test with errors.Is.
var ErrInvalidUTF8 = lib0.ErrInvalidUTF8

// ErrSplitSurrogate is wrapped by the error ApplyUpdateV2 and MergeUpdatesV2
// return for a V2 update whose string column cuts a surrogate pair in two:
// two JS strings, one ending in a lone high surrogate and the next starting
// with a lone low one, which yjs accepts. A Go string cannot hold a lone
// surrogate, and replacing the halves with U+FFFD would change keys and
// names yjs keeps apart, so ygo refuses the update. The V1 encoding of the
// same document (which writes the halves as U+FFFD) applies.
var ErrSplitSurrogate = lib0.ErrSplitSurrogate

// ErrNestedTooDeep is wrapped by the errors ApplyUpdate, ApplyUpdateV2,
// ValidateUpdate, MergeUpdates and MergeUpdatesV2 return for an update
// holding a value whose arrays and objects nest deeper than 10000 levels,
// the bound encoding/json applies to JSON text. yjs itself cannot read
// such a value (lib0 readAny overflows the JavaScript stack after about
// 4,700 levels in Node 26); without the bound, a few megabytes of nested
// arrays overflowed the goroutine stack, a crash no caller can recover
// from. Test with errors.Is.
var ErrNestedTooDeep = encoding.ErrAnyTooDeep

// ErrUpdateTooDense is wrapped by the error ApplyUpdateV2 and
// MergeUpdatesV2 return for a V2 update that expands past what its length
// pays for: more structs than max(its length in bytes, 1048576), counted
// client by client before each client's structs are decoded, or a cached
// key read again for more bytes than that. V2 run-length encodes most of
// a struct, so a valid update can hold more structs than bytes (a million
// deleted characters fit in 42); the floor keeps what such an update may
// cost to what the largest V1 message the server takes already costs.
// Test with errors.Is.
var ErrUpdateTooDense = encoding.ErrUpdateTooDense

// ErrUndefinedEmbed is returned by Text.InsertEmbed and
// InsertEmbedWithAttributes for an embed that is Undefined{} itself, before
// anything changes. yjs throws on insertEmbed(undefined). An undefined embed
// a peer sent (V2) stays in the document and reads as Undefined{}.
var ErrUndefinedEmbed = types.ErrUndefinedEmbed

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
// no equivalent strict check for V2 bytes. V2 bytes that decode within their
// own length are reported as V2; a V2 update holding more structs than bytes
// is not expanded to tell, and is reported as trailing bytes.
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
		if encoding.LooksLikeUpdateV2(raw) {
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

// MissingSV returns the wire-encoded V1 state vector to ask a peer
// with for what d's pending buffer waits on: for each client a queued
// block or delete needs, the clock d has for it. A peer's EncodeDiff
// against it carries everything from there on. An empty result means
// the queue is empty.
//
// Sync-protocol callers send this to peers as a re-fetch request, in
// place of the document's state vector.
func MissingSV(d *Doc) []byte {
	t := d.ReadTxn()
	defer t.Close()
	sv := encoding.MissingSV(t)
	return encoding.EncodeStateVector(sv, nil)
}

// MergeUpdates coalesces V1 update blobs into a single equivalent V1
// update. Returns nil for an empty input - nil is not itself a valid
// update, so guard the length before feeding the result onward.
//
// Nothing is dropped. A block whose causal dependencies are absent from
// the merged set is preserved, and the clocks nobody supplied come out as
// Skip runs, which is how the wire format expresses a hole and what yjs
// mergeUpdates emits for the same input. Two consequences worth knowing:
// the output may contain Skip runs where an ordinary update would not, and
// applying it to an empty document may leave part of it queued in the
// pending buffer until the missing history arrives. Carrying is not
// integrating.
//
// A single update comes back as it is, once it decodes, as yjs
// mergeUpdates returns it.
//
// Unlike yjs mergeUpdates, which merges the updates' bytes without
// integrating them, MergeUpdates integrates them into a scratch document
// and writes its state: what integrates comes out as the document holds
// it (one run of a client's consecutive characters, say, where yjs keeps
// the runs the updates had), and what waits comes out as yjs's merge of
// the updates two at a time, which can differ from its merge of three or
// more at once where they cover the same clocks (docs/tech-debt.md).
//
// Used by persistence layers for compaction (Flush) and by transports
// that want to batch-coalesce updates before sending.
func MergeUpdates(updates [][]byte) ([]byte, error) {
	// Use the persist package's helper directly — the function is
	// stateless and re-exporting it would create a dependency cycle
	// between root ygo and persist. Inline the small wrapper here.
	if len(updates) == 0 {
		return nil, nil
	}
	if len(updates) == 1 {
		if _, _, err := encoding.DecodeUpdate(updates[0]); err != nil {
			return nil, err
		}
		return append([]byte(nil), updates[0]...), nil
	}
	// GC off: a merge carries deleted content, as yjs mergeUpdates does;
	// whoever applies the result collects it, or keeps it with GC off.
	d := doc.NewDocWithOptions(doc.Options{DisableGC: true})
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
	return encoding.EncodeStateAsUpdateWithPending(d), nil
}

// MergeUpdatesV2 is the V2 counterpart of MergeUpdates, with the same
// contract: nothing is dropped, holes come out as Skip runs, the result
// may not integrate fully into an empty document until the missing
// history arrives, and a single update comes back as it is. Returns nil
// for an empty input.
//
// The two formats carry a Skip's length differently - V2 puts it in the
// rest stream rather than the len column - so the emitters are separate
// code paths with separate tests.
func MergeUpdatesV2(updates [][]byte) ([]byte, error) {
	return mergeUpdatesV2(updates, encoding.V2StructFloor)
}

// mergeUpdatesV2 is MergeUpdatesV2 with floor in place of V2StructFloor
// (tests of the bounds past the floor use a small one).
func mergeUpdatesV2(updates [][]byte, floor int) ([]byte, error) {
	if len(updates) == 0 {
		return nil, nil
	}
	if len(updates) == 1 {
		if _, err := encoding.DecodeUpdateV2Floor(updates[0], floor); err != nil {
			return nil, err
		}
		return append([]byte(nil), updates[0]...), nil
	}
	// GC off: a merge carries deleted content, as yjs mergeUpdates does;
	// whoever applies the result collects it, or keeps it with GC off.
	// Each input is decoded within its own budget (DecodeUpdateV2), one at
	// a time; what the merge holds is bounded as one update of all the
	// inputs' bytes would be: max(their length, V2StructFloor) structs.
	// Structs an input repeats are not held twice.
	total := 0
	for _, raw := range updates {
		total += len(raw)
	}
	held := max(total, floor)
	d := doc.NewDocWithOptions(doc.Options{DisableGC: true})
	wtxn := d.WriteTxn()
	for _, raw := range updates {
		upd, err := encoding.DecodeUpdateV2Floor(raw, floor)
		if err != nil {
			wtxn.Commit()
			return nil, err
		}
		if err := upd.Apply(wtxn); err != nil {
			wtxn.Commit()
			return nil, err
		}
		if encoding.TxnHeldStructs(wtxn) > held {
			wtxn.Commit()
			return nil, fmt.Errorf("ygo: MergeUpdatesV2: the inputs hold more than %d structs: %w", held, ErrUpdateTooDense)
		}
	}
	wtxn.Commit()
	out := encoding.EncodeStateAsUpdateV2WithPending(d)
	// The merge must be an update ApplyUpdateV2 takes: merged runs of
	// byteless structs compress, so the output can hold more structs than
	// max(its own length, V2StructFloor) though every input did not.
	if encoding.StateStructBound(d) > max(len(out), floor) {
		if _, err := encoding.DecodeUpdateV2Floor(out, floor); err != nil {
			return nil, fmt.Errorf("ygo: MergeUpdatesV2: the merged update: %w", err)
		}
	}
	return out, nil
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
// It reconstructs the update's state and diffs against remoteSV, cutting
// the first block of each client at the remote clock like EncodeDiff, and
// carries the blocks whose dependencies are absent from the input, as yjs
// diffUpdate does. The update is integrated first, into a document with
// garbage collection on, so blocks commit-time squash can merge come out
// merged, and deleted content comes out as ContentDeleted, where yjs
// diffUpdate keeps the input's blocks as they are.
func DiffUpdate(update, remoteSV []byte) ([]byte, error) {
	d := NewDoc()
	if err := ApplyUpdate(d, update); err != nil {
		return nil, err
	}
	return EncodeDiff(d, remoteSV)
}

// JSONValue returns v as JSON.stringify sees it, for encoding/json: an
// undefined object entry is dropped, an undefined anywhere else is null,
// -0 is 0, and a BigInt is a JSON number with all its digits (where
// JSON.stringify throws). Containers are copied; v is not changed. Values
// that are not Any trees pass through.
func JSONValue(v any) any { return anyval.ForJSON(v) }
