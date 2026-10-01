// Package doc owns the Doc and Transaction types — the document
// container plus its mutation-lifecycle wrapper.
//
// A Doc is the smallest self-contained CRDT replica: it holds a
// client identifier, a block store of every Item this replica has
// produced or received, and a single RWMutex that gates concurrent
// access. Mutations occur inside a Transaction (write) or are
// observed inside a Transaction (read).
//
// See docs/yrs-port-notes/transaction.md for the per-method contract
// and the 11-step commit lifecycle yrs runs at TransactionMut.Commit.
// Most of that lifecycle is not yet implemented; see tech-debt.md.
package doc

import (
	"crypto/rand"
	"encoding/binary"
	"sync"

	"github.com/Deln0r/ygo/internal/block"
	"github.com/Deln0r/ygo/internal/store"
)

// MaxClientID is the upper bound on Doc.ClientID values. Set to
// 2^53-1 to match the JS-safe-integer range that JS Yjs uses for its
// clientID; all values produced by NewDoc fit in [1, MaxClientID].
const MaxClientID uint64 = (1 << 53) - 1

// Options bundles per-Doc settings. The zero value (Options{}) gives
// the safe defaults that JS Yjs and yrs use: GC enabled, random
// ClientID. Fields are inverted from their natural English where
// needed so that Go's zero-value default matches the recommended
// configuration.
type Options struct {
	// DisableGC turns OFF garbage collection of fully-observed
	// deleted items at transaction commit. Default (false) means GC
	// is enabled, matching JS Yjs and yrs. Set to true only if you
	// intend to use snapshots; otherwise GC may drop content the
	// snapshot needs to materialize.
	DisableGC bool

	// ClientID overrides the random client identifier. When zero
	// (the default), NewDocWithOptions generates one via crypto/rand.
	// Pass a fixed value only for deterministic tests.
	ClientID uint64

	// GUID is the document's stable string identity. Subdocuments are
	// referenced by GUID on the wire. When empty (the default), a
	// random GUID is generated. Two Docs sharing a GUID are treated as
	// the same logical (sub)document.
	GUID string
}

// Doc is a single CRDT replica. Construct with NewDoc.
//
// Doc is safe for concurrent access through ReadTxn / WriteTxn — those
// methods acquire the internal RWMutex appropriately. Direct field
// access from outside the doc package is not safe.
type Doc struct {
	clientID     uint64
	guid         string
	store        *store.BlockStore
	gc           bool
	mu           sync.RWMutex
	rootBranches map[string]*block.Branch

	// subdocs maps a subdocument GUID to its in-memory Doc handle, so
	// repeated lookups of the same nested document return one
	// instance. Populated when a ContentDoc is created locally or
	// surfaced from a decoded update.
	//
	// Guarded by its own subdocsMu, NOT the doc write lock: SetDoc
	// registers a subdoc from inside a write transaction (which holds
	// d.mu), so reusing d.mu here would deadlock. The registry is an
	// independent concern from the block store.
	subdocs   map[string]*Doc
	subdocsMu sync.Mutex

	// shouldLoad reports whether this (sub)document's content should be
	// loaded/synced. Set by Load or by the autoLoad option on the
	// referencing ContentDoc. Drives the subdocsLoaded set. Guarded by
	// subdocsMu.
	shouldLoad bool

	// pendingState is an opaque pointer the encoding layer uses to
	// store a *encoding.Pending value (queued blocks and delete-set
	// entries awaiting unresolved dependencies). We keep the type
	// any here so the doc package does not import encoding —
	// encoding already imports doc, so the reverse direction would
	// be a cycle. Read and written under the same write lock that
	// guards the store (see PendingState / SetPendingState on
	// TransactionMut).
	pendingState any

	// handlersMu guards the handler lists below, which Commit runs in
	// registration order before the write lock is released. It is not
	// the doc lock: Commit calls the handlers with the doc lock held,
	// and a handler may register or unsubscribe one, itself included,
	// or close an UndoManager, from inside the call. Commit runs a
	// snapshot taken under handlersMu.
	handlersMu sync.Mutex

	// afterTxnHandlers fire at the end of TransactionMut.Commit, after
	// the write-lock state is finalised. UndoManager and any other
	// observer (logging, sync provider broadcast, etc.) subscribe via
	// OnAfterTransaction.
	afterTxnHandlers []*registeredAfterTxn

	// updateHandlers / updateV2Handlers receive each committed
	// transaction's update (OnUpdate / OnUpdateV2).
	updateHandlers   []*registeredUpdate
	updateV2Handlers []*registeredUpdate
}

// registeredAfterTxn wraps a handler so each registration has a
// stable identity for unsubscribe. Function values are not directly
// comparable in Go; comparing pointers to the wrapper sidesteps that.
type registeredAfterTxn struct {
	fn AfterTransactionHandler
}

// AfterTransactionHandler is invoked once per committed write
// transaction, after the change-tracking state on TransactionMut has
// been finalised. The handler runs with the doc's write lock still
// held; it must not acquire ReadTxn or WriteTxn on the same doc.
// Heavy or blocking work should be dispatched to a goroutine.
type AfterTransactionHandler func(*TransactionMut)

// OnAfterTransaction registers fn to fire at the end of every future
// TransactionMut.Commit. Returns an unsubscribe function that removes
// the handler; calling it more than once is a no-op.
//
// Registration order is preserved; handlers fire in registration
// order. A handler may register or unsubscribe handlers, itself
// included, or close an UndoManager, from inside the call; the change
// applies from the next transaction. Panics in a handler propagate
// and are NOT caught — they leave the doc write lock held by the
// panicking goroutine, which is the same behaviour as any other panic
// during Commit.
func (d *Doc) OnAfterTransaction(fn AfterTransactionHandler) func() {
	h := &registeredAfterTxn{fn: fn}
	d.handlersMu.Lock()
	d.afterTxnHandlers = append(d.afterTxnHandlers, h)
	d.handlersMu.Unlock()
	return func() {
		d.handlersMu.Lock()
		defer d.handlersMu.Unlock()
		for i, r := range d.afterTxnHandlers {
			if r == h {
				d.afterTxnHandlers = append(d.afterTxnHandlers[:i], d.afterTxnHandlers[i+1:]...)
				return
			}
		}
	}
}

// fireAfterTransactionHandlers runs every registered handler with
// the given mut. Called from TransactionMut.Commit while holding the
// write lock. Snapshot the slice first so a handler that calls
// OnAfterTransaction (registers a new observer) does not see itself
// added mid-iteration.
func (d *Doc) fireAfterTransactionHandlers(mut *TransactionMut) {
	d.handlersMu.Lock()
	handlers := make([]*registeredAfterTxn, len(d.afterTxnHandlers))
	copy(handlers, d.afterTxnHandlers)
	d.handlersMu.Unlock()
	for _, h := range handlers {
		h.fn(mut)
	}
}

// UpdateHandler receives one committed transaction as an update and the
// transaction's Origin. The bytes are shared by every handler of that
// transaction; do not modify them.
type UpdateHandler func(update []byte, origin any)

// registeredUpdate gives each update registration a stable identity for
// unsubscribe, as registeredAfterTxn does.
type registeredUpdate struct {
	fn UpdateHandler
}

// UpdateEncoder encodes a committed transaction as the update yjs emits
// for it ('update' when v2 is false, 'updateV2' when true), or returns
// nil when the transaction changed nothing. The encoding package
// installs it in an init: doc cannot import encoding, which imports
// doc. Every package that exposes OnUpdate imports encoding.
var UpdateEncoder func(t *TransactionMut, v2 bool) []byte

// OnUpdate registers fn to receive, after every write transaction that
// changed the document, a V1 update holding exactly that transaction's
// changes, and the transaction's Origin. It is the yjs
// doc.on('update') event: structs created from the transaction's start
// state on (the first of each client cut at that clock) and the delete
// set of what the transaction deleted, byte for byte as yjs writes them
// apart from three pinned cases (docs/tech-debt.md, "Remaining
// update-event divergences": deleting a nested type lists only the
// type's own item, yjs's extra cleanup update after concurrent
// formatting is not emitted, undo restores kept tombstones as separate
// structs) and the Any values ygo writes differently (objects with
// several keys, written with sorted keys, and whole-number float64s).
// Local edits, applied updates and UndoManager steps all emit one; a
// transaction that changed nothing emits none.
//
// fn runs at the end of Commit, after observers, after-transaction
// handlers and garbage collection, with the write lock still held, so
// updates arrive in commit order even with concurrent writers. Like an
// after-transaction handler it must not open a transaction on the same
// doc; send or queue the bytes. It may register or unsubscribe
// handlers, itself included, or close an UndoManager; a change applies
// from the next transaction. Returns an unsubscribe function; calling
// it more than once is a no-op.
func (d *Doc) OnUpdate(fn UpdateHandler) func() {
	return d.addUpdateHandler(&d.updateHandlers, fn)
}

// OnUpdateV2 is OnUpdate with V2 wire bytes, the yjs
// doc.on('updateV2') event.
func (d *Doc) OnUpdateV2(fn UpdateHandler) func() {
	return d.addUpdateHandler(&d.updateV2Handlers, fn)
}

func (d *Doc) addUpdateHandler(list *[]*registeredUpdate, fn UpdateHandler) func() {
	h := &registeredUpdate{fn: fn}
	d.handlersMu.Lock()
	*list = append(*list, h)
	d.handlersMu.Unlock()
	return func() {
		d.handlersMu.Lock()
		defer d.handlersMu.Unlock()
		for i, r := range *list {
			if r == h {
				*list = append((*list)[:i:i], (*list)[i+1:]...)
				return
			}
		}
	}
}

// fireUpdateHandlers encodes the committed transaction once per wire
// version that has a handler and calls those handlers in registration
// order. Called from Commit under the write lock. Both handler lists
// are copied before any call, so handlers can change them.
func (d *Doc) fireUpdateHandlers(t *TransactionMut) {
	if UpdateEncoder == nil {
		return
	}
	d.handlersMu.Lock()
	v1 := append([]*registeredUpdate(nil), d.updateHandlers...)
	v2 := append([]*registeredUpdate(nil), d.updateV2Handlers...)
	d.handlersMu.Unlock()
	for i, handlers := range [][]*registeredUpdate{v1, v2} {
		if len(handlers) == 0 {
			continue
		}
		update := UpdateEncoder(t, i == 1)
		if update == nil {
			return // the transaction changed nothing, in either version
		}
		for _, h := range handlers {
			h.fn(update, t.Origin)
		}
	}
}

// NewDoc returns a fresh Doc with default options and a random
// client identifier in [1, MaxClientID].
func NewDoc() *Doc {
	return NewDocWithOptions(Options{})
}

// NewDocWithOptions returns a fresh Doc with the given options.
// A zero Options{} is equivalent to NewDoc().
func NewDocWithOptions(opts Options) *Doc {
	cid := opts.ClientID
	if cid == 0 {
		cid = newClientID()
	}
	guid := opts.GUID
	if guid == "" {
		guid = newGUID()
	}
	return &Doc{
		clientID:     cid,
		guid:         guid,
		store:        store.NewBlockStore(),
		gc:           !opts.DisableGC,
		rootBranches: map[string]*block.Branch{},
		subdocs:      map[string]*Doc{},
	}
}

// newGUID returns a random hex identifier for a (sub)document,
// mirroring yjs's default random-string guid.
func newGUID() string {
	var b [11]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "00000000000000000000"
	}
	const hexdigits = "0123456789abcdef"
	out := make([]byte, 22)
	for i, x := range b {
		out[i*2] = hexdigits[x>>4]
		out[i*2+1] = hexdigits[x&0x0f]
	}
	return string(out[:21])
}

// GUID returns the document's stable string identity.
func (d *Doc) GUID() string { return d.guid }

// Subdoc returns the in-memory handle for the subdocument with the
// given GUID, creating and registering one (GC disabled, matching
// subdoc semantics) on first access. Safe for concurrent use.
func (d *Doc) Subdoc(guid string) *Doc {
	d.subdocsMu.Lock()
	defer d.subdocsMu.Unlock()
	if d.subdocs == nil {
		d.subdocs = map[string]*Doc{}
	}
	if sub, ok := d.subdocs[guid]; ok {
		return sub
	}
	sub := NewDocWithOptions(Options{GUID: guid, DisableGC: true})
	d.subdocs[guid] = sub
	return sub
}

// PutSubdoc registers an existing Doc as a subdocument under its GUID,
// so later Subdoc / GetDoc lookups return the same instance. A no-op if
// a different instance is already registered for that GUID.
func (d *Doc) PutSubdoc(sub *Doc) {
	d.subdocsMu.Lock()
	defer d.subdocsMu.Unlock()
	if d.subdocs == nil {
		d.subdocs = map[string]*Doc{}
	}
	if _, ok := d.subdocs[sub.guid]; !ok {
		d.subdocs[sub.guid] = sub
	}
}

// RemoveSubdoc drops the in-memory handle for the subdocument with the
// given GUID from the registry, so later Subdoc / GetDoc lookups no
// longer return a stale instance. Called when a ContentDoc reference is
// tombstoned. A no-op if no handle is registered. Safe for concurrent
// use. CRDT state is unaffected; this only releases the cached handle.
func (d *Doc) RemoveSubdoc(guid string) {
	d.subdocsMu.Lock()
	defer d.subdocsMu.Unlock()
	delete(d.subdocs, guid)
}

// SubdocsEvent carries the subdocument lifecycle changes observed in a
// single transaction: the GUIDs added (a ContentDoc surfaced), removed
// (its reference was tombstoned), and loaded (autoLoad or an explicit
// Load). Mirrors yjs's "subdocs" event payload.
type SubdocsEvent struct {
	Added   []string
	Removed []string
	Loaded  []string
}

// OnSubdocs registers fn to fire after any transaction that changed the
// document's subdocuments. Returns an unsubscribe function. A sync
// provider uses this to learn which nested documents to start or stop
// syncing. Convenience layer over OnAfterTransaction.
func (d *Doc) OnSubdocs(fn func(SubdocsEvent)) func() {
	return d.OnAfterTransaction(func(t *TransactionMut) {
		if len(t.subdocsAdded) == 0 && len(t.subdocsRemoved) == 0 && len(t.subdocsLoaded) == 0 {
			return
		}
		fn(SubdocsEvent{
			Added:   t.subdocsAdded,
			Removed: t.subdocsRemoved,
			Loaded:  t.subdocsLoaded,
		})
	})
}

// Load marks this subdocument to be loaded. A provider watching the
// parent's transactions sees the GUID in SubdocsLoaded and can then
// fetch and apply the subdocument's own update stream. Idempotent.
func (d *Doc) Load() {
	d.subdocsMu.Lock()
	d.shouldLoad = true
	d.subdocsMu.Unlock()
}

// ShouldLoad reports whether this (sub)document is marked to load.
func (d *Doc) ShouldLoad() bool {
	d.subdocsMu.Lock()
	defer d.subdocsMu.Unlock()
	return d.shouldLoad
}

// Subdocs returns the GUIDs of all registered subdocuments.
func (d *Doc) Subdocs() []string {
	d.subdocsMu.Lock()
	defer d.subdocsMu.Unlock()
	out := make([]string, 0, len(d.subdocs))
	for g := range d.subdocs {
		out = append(out, g)
	}
	return out
}

// ClientID returns the random per-replica identifier. Stable for the
// life of the Doc; used as the Client field in every ID this replica
// produces.
func (d *Doc) ClientID() uint64 { return d.clientID }

// GC reports whether garbage collection of fully-observed deleted
// items will run at transaction commit.
func (d *Doc) GC() bool { return d.gc }

// Branch returns the root branch with the given name, creating it
// lazily on first access. The returned *block.Branch is the
// underlying state shared by all types-layer wrappers (Map, Array,
// Text) constructed against this name on this Doc.
//
// Concurrency: Branch acquires the doc's write lock to insert into
// the root-branch registry. It MUST be called outside any active
// Transaction or TransactionMut on this Doc — calling it inside a
// transaction deadlocks (sync.RWMutex is non-reentrant).
//
// Typical usage:
//
//	d := doc.NewDoc()
//	settingsBranch := d.Branch("settings")  // outside any txn
//	m := types.NewMap(settingsBranch)        // wrap once
//	// later, inside a txn:
//	txn := d.WriteTxn()
//	m.Set(txn, "color", "red")
//	txn.Commit()
func (d *Doc) Branch(name string) *block.Branch {
	d.mu.Lock()
	defer d.mu.Unlock()
	if b, ok := d.rootBranches[name]; ok {
		return b
	}
	b := &block.Branch{Name: name, Map: map[string]*block.Item{}}
	d.rootBranches[name] = b
	return b
}

// newClientID returns a random non-zero uint64 in [1, MaxClientID].
// Uses crypto/rand for collision resistance across replicas. Retry
// loop handles the (probability 2^-53) case where the masked draw is
// zero, which our convention treats as reserved (yrs uses NonZeroU64).
func newClientID() uint64 {
	var b [8]byte
	for {
		if _, err := rand.Read(b[:]); err != nil {
			panic("ygo: crypto/rand failed: " + err.Error())
		}
		id := binary.BigEndian.Uint64(b[:]) & MaxClientID
		if id != 0 {
			return id
		}
	}
}
