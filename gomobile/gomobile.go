// Package gomobile is the bytes-in/bytes-out subset of the ygo API
// designed to survive `gomobile bind`. The full public API at
// github.com/Deln0r/ygo exposes Go types that gomobile cannot
// bind (the `any` interface, callbacks, generic maps), so iOS /
// Android consumers import this package instead and exchange
// state with their UI layer via byte arrays.
//
// What gomobile filters out and we work around:
//
//   - Function parameters of `any` / `map[K]V` / `chan T` / `func(...)`
//     are silently skipped from the generated binding. ygo.Map.Set
//     takes `value any`; gomobile produces a Map type with no Set.
//   - Slices of non-byte types (e.g. `[]any`, `[]string`) are
//     skipped. ygo.Array.ToSlice() returns `[]any`.
//   - Generics break the bind step entirely.
//   - Callback registration (Sub, OnUpdate, OnChange) is skipped;
//     this package takes listener interfaces instead (ObserveUpdates,
//     ObserveChanges), which gomobile does bind.
//
// What survives:
//
//   - Opaque struct pointers (Doc, this package's wrapper types).
//   - Methods that take/return `string`, `int*`/`uint*`, `bool`,
//     `float*`, `[]byte`, and `error`.
//   - Bytes-in/bytes-out wire-format operations: this package
//     exposes those plus a tiny Doc wrapper.
//
// The package exposes two API levels:
//
//   - App level (types.go, client.go): Text (insert / delete /
//     cursors), Map (string keys and values), UndoManager, and
//     Client — a complete background sync provider (WebSocket,
//     handshake, reconnect) so a Swift / Kotlin app only renders UI
//     and edits the Doc. Reads run under internal read transactions,
//     safe against the background client.
//
//   - Wire level (this file): bytes-in/bytes-out y-protocols flow —
//     encode local state to bytes, apply remote bytes, bring your
//     own transport.
//
// gomobile bind verification: actual `gomobile bind -target=ios`
// or `-target=android` requires the corresponding toolchain (Xcode
// / Android NDK) and is not run in CI. The package compiles under
// pure Go to guarantee no inadvertent CGO leak via dependencies.
package gomobile

import (
	"fmt"
	"sync"

	"github.com/Deln0r/ygo/internal/awareness"
	"github.com/Deln0r/ygo/internal/doc"
	"github.com/Deln0r/ygo/internal/encoding"
	"github.com/Deln0r/ygo/internal/store"
)

// Doc is the gomobile-bindable opaque handle for a ygo CRDT
// replica. Construct via NewDoc / NewDocWithClientID. All
// shared-type operations route through wire-format bytes — push
// updates in via ApplyUpdate, pull state out via
// EncodeStateAsUpdate.
//
// Concurrency: safe for use from multiple goroutines /
// gomobile-bound callers. Each method opens its own transaction
// internally; gomobile callers do not see Transaction or
// TransactionMut directly.
type Doc struct {
	inner *doc.Doc
	// origin tags every edit made through this handle and the shared
	// types taken from it ("" for none); see WithOrigin.
	origin string
	shared *docShared
}

// docShared is the state every handle of one document shares: the
// current update listener, and the one document handler that calls it,
// registered once. Replacing the listener swaps the pointer under mu,
// so every update reaches exactly one listener, the current one;
// unregistering and registering a handler per listener left a gap in
// which a commit reached neither.
type docShared struct {
	mu              sync.Mutex
	updateListener  UpdateListener
	updatesObserved bool
}

// NewDoc returns a fresh Doc with a random ClientID.
func NewDoc() *Doc {
	return &Doc{inner: doc.NewDoc(), shared: &docShared{}}
}

// WithOrigin returns a handle on the same document whose edits carry
// origin: every Text, Map, Array, rich-text and XML mutation made through
// it, or through a shared type taken from it, commits with that origin.
// An UndoManager created from it tracks that origin, so
//
//	local := doc.WithOrigin("local")
//	um := local.NewTextUndoManager("body")
//	local.Text("body").InsertAt(0, "hi")             // captured
//	doc.ApplyUpdateWithOrigin(fromPeer, "remote")  // not captured
//
// undoes the user's own typing and never a collaborator's. The handle's
// origin is fixed, so concurrent callers using different handles cannot
// mislabel each other's edits. "" is the plain handle, no origin.
func (d *Doc) WithOrigin(origin string) *Doc {
	return &Doc{inner: d.inner, origin: origin, shared: d.shared}
}

// writeTxn opens a write transaction carrying this handle's origin.
func (d *Doc) writeTxn() *doc.TransactionMut {
	txn := d.inner.WriteTxn()
	txn.Origin = originValue(d.origin)
	return txn
}

// originValue maps the string origins of this package to transaction
// origins: "" is no origin (nil).
func originValue(origin string) any {
	if origin == "" {
		return nil
	}
	return origin
}

// NewDocWithClientID returns a fresh Doc with the given ClientID.
// Use this when the calling application wants deterministic
// per-device IDs (typical mobile pattern: derive from a stable
// device identifier).
func NewDocWithClientID(clientID uint64) *Doc {
	return &Doc{inner: doc.NewDocWithOptions(doc.Options{ClientID: clientID}), shared: &docShared{}}
}

// ClientID returns this replica's client identifier.
func (d *Doc) ClientID() uint64 { return d.inner.ClientID() }

// ApplyUpdate decodes raw V1 wire bytes and integrates them into
// the doc. Items whose dependencies the local store has not yet
// seen queue in the per-doc pending buffer and integrate
// automatically on subsequent ApplyUpdate calls that satisfy them.
//
// Returns an error only for malformed wire bytes; missing-
// dependency cases queue silently and are queryable via HasPending
// / MissingSV.
func (d *Doc) ApplyUpdate(rawBytes []byte) error {
	if err := encoding.ApplyUpdate(d.inner, rawBytes); err != nil {
		return fmt.Errorf("ApplyUpdate: %w", err)
	}
	return nil
}

// ApplyUpdateWithOrigin is ApplyUpdate with origin as the transaction's
// origin, whatever the handle's own. Apply a collaborator's updates with
// an origin of their own ("remote", say): plain ApplyUpdate commits with
// no origin, which a default UndoManager captures, so Undo would revert
// the collaborator's edit. Update listeners receive the origin too.
func (d *Doc) ApplyUpdateWithOrigin(rawBytes []byte, origin string) error {
	if err := encoding.ApplyUpdateWithOrigin(d.inner, rawBytes, originValue(origin)); err != nil {
		return fmt.Errorf("ApplyUpdate: %w", err)
	}
	return nil
}

// UpdateListener receives each committed transaction as a V1 update.
// Implement it in Swift / Kotlin and pass it to Doc.ObserveUpdates.
type UpdateListener interface {
	OnUpdate(update []byte, origin string)
}

// ObserveUpdates registers l to receive, after every transaction that
// changes the document, a V1 update holding exactly that transaction's
// changes (the yjs doc.on('update') event): send it to peers or append
// it to a log. origin is the transaction's origin when it is a string
// (WithOrigin, ApplyUpdateWithOrigin) and "" otherwise, including undo
// steps and the built-in Client's transactions.
//
// One listener per document: this replaces the listener registered
// before through any handle of it, and nil detaches. Like the change
// listeners it is called while the document lock is held: hand the
// bytes off and return, do not call back into the document
// synchronously.
func (d *Doc) ObserveUpdates(l UpdateListener) {
	sh := d.shared
	sh.mu.Lock()
	defer sh.mu.Unlock()
	sh.updateListener = l
	if sh.updatesObserved || l == nil {
		return
	}
	sh.updatesObserved = true
	d.inner.OnUpdate(func(update []byte, origin any) {
		sh.mu.Lock()
		cur := sh.updateListener
		sh.mu.Unlock()
		if cur != nil {
			s, _ := origin.(string)
			cur.OnUpdate(update, s)
		}
	})
}

// EncodeStateAsUpdate returns wire-encoded V1 bytes carrying the
// doc's full state. Apply to a peer doc to bring it up to speed.
// Interoperates with JS Yjs Y.encodeStateAsUpdate output.
func (d *Doc) EncodeStateAsUpdate() []byte {
	return encoding.EncodeStateAsUpdate(d.inner)
}

// EncodeStateVector returns wire-encoded V1 state vector — what
// the y-sync protocol's SyncStep1 sends. The peer replies with
// the diff.
func (d *Doc) EncodeStateVector() []byte {
	t := d.inner.ReadTxn()
	defer t.Close()
	return encoding.EncodeStateVector(t.Store().GetStateVector(), nil)
}

// EncodeDiff returns the wire-encoded V1 update carrying blocks
// this doc has that the remote (per remoteSV bytes) does not. A
// nil remoteSV is treated as the empty state vector — emit
// everything.
//
// remoteSV is the V1 wire-encoded form of the remote's state
// vector (same shape EncodeStateVector produces). Pass straight
// from a sync-protocol message; the function decodes internally.
func (d *Doc) EncodeDiff(remoteSV []byte) ([]byte, error) {
	var sv store.StateVector
	if len(remoteSV) > 0 {
		decoded, _, err := encoding.DecodeStateVector(remoteSV)
		if err != nil {
			return nil, fmt.Errorf("EncodeDiff: remoteSV decode: %w", err)
		}
		sv = decoded
	}
	t := d.inner.ReadTxn()
	defer t.Close()
	return encoding.EncodeDiff(d.inner, t, sv), nil
}

// HasPending reports whether the doc has any queued items
// awaiting causal dependencies (items that arrived via ApplyUpdate
// before their Origin / RightOrigin / Parent ID was visible
// locally).
func (d *Doc) HasPending() bool {
	t := d.inner.ReadTxn()
	defer t.Close()
	return encoding.HasPending(t)
}

// MissingSV returns the wire-encoded V1 state vector identifying
// the clocks this doc needs to receive in order to drain its
// pending buffer. Send to a peer as a re-fetch request.
//
// An empty return ([]byte{0x00}, the encoded-empty-SV) means the
// pending buffer is empty.
func (d *Doc) MissingSV() []byte {
	t := d.inner.ReadTxn()
	defer t.Close()
	sv := encoding.MissingSV(t)
	return encoding.EncodeStateVector(sv, nil)
}

// Awareness is the gomobile-bindable handle for the y-protocols
// Awareness presence layer. Independent of any Doc; the local
// clientID is passed at construction. Embedders typically pair
// an Awareness with a Doc and use d.ClientID() as the awareness
// clientID.
//
// All state is exchanged as raw JSON bytes. Mobile UIs that need
// typed access (cursor positions, user names) JSON-parse on the
// caller side after pulling via States.
type Awareness struct {
	inner *awareness.Awareness
}

// NewAwareness returns a fresh Awareness for the given clientID.
func NewAwareness(clientID uint64) *Awareness {
	return &Awareness{inner: awareness.New(clientID)}
}

// ClientID returns the local awareness clientID.
func (a *Awareness) ClientID() uint64 { return a.inner.ClientID() }

// SetLocalState replaces the local client's awareness state with
// the given raw JSON bytes. Nil clears the state (equivalent to
// RemoveLocalState). The clock bumps on every call — even no-op
// equal sets — per the y-protocols 15-second heartbeat invariant.
func (a *Awareness) SetLocalState(jsonBytes []byte) {
	a.inner.SetLocalState(jsonBytes)
}

// LocalState returns the local client's current state JSON bytes,
// or nil if the state has not been set / has been removed.
func (a *Awareness) LocalState() []byte {
	state, ok := a.inner.LocalState()
	if !ok {
		return nil
	}
	return state
}

// RemoveLocalState marks the local client offline. The wire-
// format representation is an entry with JSON "null"; the local
// entry is kept (with state cleared) so the clock survives
// subsequent revival races.
func (a *Awareness) RemoveLocalState() {
	a.inner.RemoveLocalState()
}

// Encode returns the V1 wire bytes for the awareness states of
// the given clientIDs. Pass an empty clientIDs slice to encode
// every known client.
//
// Note: gomobile-bound callers cannot pass `[]uint64` directly
// (slices of non-byte types are skipped). Use EncodeAll for the
// common "send everything" case; for per-client encoding adopters
// must wrap one ID at a time or extend this package.
func (a *Awareness) EncodeAll() []byte {
	return a.inner.Encode(nil)
}

// Apply integrates a wire-encoded awareness update. The origin
// string is forwarded to subscribers — but since callback
// registration is not gomobile-bindable, mobile callers typically
// pass an empty string.
//
// Returns an error only for malformed wire bytes.
func (a *Awareness) Apply(rawBytes []byte, origin string) error {
	_, err := a.inner.Apply(rawBytes, origin)
	if err != nil {
		return fmt.Errorf("Apply: %w", err)
	}
	return nil
}
