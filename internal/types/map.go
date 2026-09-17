package types

import (
	"github.com/Deln0r/ygo/internal/block"
	"github.com/Deln0r/ygo/internal/doc"
)

// Map is the user-facing wrapper around a map-like Branch. Construct
// via NewMap; usually obtained as types.NewMap(d.Branch("name")).
//
// Map exposes Set / Get / Delete / Has / Len / Range / Clear plus a
// Branch accessor for low-level integration.
type Map struct {
	branch *block.Branch
}

// NewMap wraps the given branch as a Map. The branch's Map field is
// initialized lazily if nil — typical for a freshly-created root
// branch from Doc.Branch(name).
func NewMap(branch *block.Branch) *Map {
	if branch.Map == nil {
		branch.Map = map[string]*block.Item{}
	}
	return &Map{branch: branch}
}

// Branch returns the underlying *block.Branch. Useful for
// cross-package wiring (e.g. observers, encoders) that need to
// operate at the block layer.
func (m *Map) Branch() *block.Branch { return m.branch }

// Set stores value under key in this map. Concurrent writes from
// different replicas converge to the same final value — the writer
// with the higher (clientID, clock) wins per YATA.
//
// value may be of any type; the encoding-layer port (deferred per
// tech-debt.md) will decide how each Go type maps to a wire variant.
// For now value is stored as ContentAny, which round-trips through
// reflection.
//
// Mirrors yrs Map::insert (yrs/src/types/map.rs ~line 73).
func (m *Map) Set(txn *doc.TransactionMut, key string, value any) {
	clientID := txn.Doc().ClientID()
	clock := txn.Store().GetClock(clientID)

	var origin *block.ID
	var left *block.Item
	if existing, ok := m.branch.Map[key]; ok {
		// Per types-map.md finding 1: branch.Map[key] is the tail
		// already. Per finding 2: Origin = left.LastID().
		left = existing
		lid := left.LastID()
		origin = &lid
	}

	keyCopy := key // borrow-stable string for ParentSub pointer
	item := &block.Item{
		ID:        block.ID{Client: clientID, Clock: clock},
		Len:       1,
		Origin:    origin,
		Left:      left,
		Content:   block.Content{Kind: block.KindAny, Anys: []block.Any{value}},
		Parent:    block.Parent{Kind: block.ParentBranch, Branch: m.branch},
		ParentSub: &keyCopy,
		Flags:     block.FlagCountable,
	}

	txn.Store().PushBlock(item)
	if dropped := item.Integrate(txn, 0); dropped {
		txn.Delete(item)
	}
}

// Get returns the current value under key, or nil if the key is
// absent or its tail item is tombstoned.
//
// Per types-map.md finding 5: this is one-step. branch.Map[key] is
// the LWW winner; if it's deleted there is no live predecessor to
// fall back to.
func (m *Map) Get(key string) any {
	item, ok := m.branch.Map[key]
	if !ok || item.IsDeleted() {
		return nil
	}
	return extractValue(item.Content)
}

// Has reports whether key has a live (non-tombstoned) entry.
func (m *Map) Has(key string) bool {
	item, ok := m.branch.Map[key]
	return ok && !item.IsDeleted()
}

// Delete tombstones the entry under key. Calling Delete when the
// key is absent or already tombstoned is a no-op.
//
// Per types-map.md finding 3: Delete does NOT clear branch.Map[key].
// The map entry continues to point at the tombstoned item so a
// subsequent Set can chain off it as Origin/Left, preserving YATA
// convergence for concurrent writers that didn't see the delete yet.
func (m *Map) Delete(txn *doc.TransactionMut, key string) {
	item, ok := m.branch.Map[key]
	if !ok || item.IsDeleted() {
		return
	}
	txn.Delete(item)
}

// Len returns the number of live (non-tombstoned) entries.
//
// O(N) over the map size: iterates and counts. yrs has a TODO at
// map.rs:158 about caching this on the Branch — we'd inherit that
// optimization for free if we add the cache to Branch. Tracked in
// tech-debt.md if it shows up in benchmarks.
func (m *Map) Len() int {
	n := 0
	for _, item := range m.branch.Map {
		if !item.IsDeleted() {
			n++
		}
	}
	return n
}

// Range iterates over live (key, value) pairs in unspecified order
// (Go map iteration). The callback returns false to stop early.
func (m *Map) Range(fn func(key string, value any) bool) {
	for key, item := range m.branch.Map {
		if item.IsDeleted() {
			continue
		}
		if !fn(key, extractValue(item.Content)) {
			return
		}
	}
}

// Clear deletes every entry in the map.
//
// Per types-map.md finding 4: yrs's Map::clear calls txn.delete on
// every entry, including already-tombstoned ones. Our txn.Delete is
// idempotent (early-returns on IsDeleted), so we filter here purely
// to avoid the no-op — semantics match yrs either way.
func (m *Map) Clear(txn *doc.TransactionMut) {
	for _, item := range m.branch.Map {
		if !item.IsDeleted() {
			txn.Delete(item)
		}
	}
}

// SetMap inserts a freshly-constructed nested Map under key and
// returns the wrapper bound to it. Subsequent edits via the returned
// *Map are addressed to this map-key slot in m; concurrent peers
// see the same nested-map structure once they apply the resulting
// updates.
//
// Per docs/yrs-port-notes/nested-types.md §5 — recommended API over
// overloading Set(any) so the caller never has to type-assert the
// returned wrapper.
func (m *Map) SetMap(txn *doc.TransactionMut, key string) *Map {
	inner := &block.Branch{
		TypeRef: block.TypeRefMap,
		Map:     map[string]*block.Item{},
	}
	m.setNested(txn, key, inner)
	return &Map{branch: inner}
}

// SetArray inserts a freshly-constructed nested Array under key.
func (m *Map) SetArray(txn *doc.TransactionMut, key string) *Array {
	inner := &block.Branch{TypeRef: block.TypeRefArray}
	m.setNested(txn, key, inner)
	return &Array{branch: inner}
}

// SetText inserts a freshly-constructed nested Text under key
// (plain-text only — see tech-debt.md for rich-text formatting).
func (m *Map) SetText(txn *doc.TransactionMut, key string) *Text {
	inner := &block.Branch{TypeRef: block.TypeRefText}
	m.setNested(txn, key, inner)
	return &Text{branch: inner}
}

// setNested is the shared scaffolding for SetMap/SetArray/SetText:
// build an Item with KindType content and the supplied Branch,
// chain off the existing tail at this key (Origin = left.LastID()),
// push to store, integrate. The Item.Integrate KindType arm wires
// SetDoc nests a new subdocument under key and returns its handle. The
// subdocument carries its own GUID; its content syncs as a separate
// update stream, while the parent stores only the reference (GUID +
// options). Mirrors yjs `map.set(key, new Y.Doc())`.
func (m *Map) SetDoc(txn *doc.TransactionMut, key string) *doc.Doc {
	sub := doc.NewDocWithOptions(doc.Options{DisableGC: true})
	m.setDocRefWithOpts(txn, key, sub.GUID(), map[string]any{})
	txn.Doc().PutSubdoc(sub)
	return sub
}

// SetDocWithOptions is SetDoc with subdocument options. When autoLoad
// is true the reference carries the autoLoad flag on the wire, so peers
// that decode it mark the subdocument to load automatically (it appears
// in their SubdocsLoaded set).
func (m *Map) SetDocWithOptions(txn *doc.TransactionMut, key string, autoLoad bool) *doc.Doc {
	sub := doc.NewDocWithOptions(doc.Options{DisableGC: true})
	if autoLoad {
		sub.Load()
	}
	opts := map[string]any{}
	if autoLoad {
		opts["autoLoad"] = true
	}
	m.setDocRefWithOpts(txn, key, sub.GUID(), opts)
	txn.Doc().PutSubdoc(sub)
	return sub
}

// GetDoc returns the subdocument referenced under key within parent d,
// or (nil, false) if key holds no live subdoc. The returned handle is
// the registered instance for the referenced GUID (created on first
// access for subdocs surfaced from a decoded update).
func (m *Map) GetDoc(d *doc.Doc, key string) (*doc.Doc, bool) {
	item, ok := m.branch.Map[key]
	if !ok || item == nil || item.IsDeleted() || item.Content.Kind != block.KindDoc {
		return nil, false
	}
	return d.Subdoc(item.Content.DocGuid), true
}

// setDocRefWithOpts integrates a ContentDoc reference item under key.
func (m *Map) setDocRefWithOpts(txn *doc.TransactionMut, key, guid string, opts map[string]any) {
	clientID := txn.Doc().ClientID()
	clock := txn.Store().GetClock(clientID)

	var origin *block.ID
	var left *block.Item
	if existing, ok := m.branch.Map[key]; ok {
		left = existing
		lid := left.LastID()
		origin = &lid
	}

	keyCopy := key
	item := &block.Item{
		ID:        block.ID{Client: clientID, Clock: clock},
		Len:       1,
		Origin:    origin,
		Left:      left,
		Content:   block.Content{Kind: block.KindDoc, DocGuid: guid, DocOpts: opts},
		Parent:    block.Parent{Kind: block.ParentBranch, Branch: m.branch},
		ParentSub: &keyCopy,
		Flags:     block.FlagCountable,
	}

	txn.Store().PushBlock(item)
	if dropped := item.Integrate(txn, 0); dropped {
		txn.Delete(item)
	}
}

// Branch.Item back to the Item.
func (m *Map) setNested(txn *doc.TransactionMut, key string, inner *block.Branch) {
	clientID := txn.Doc().ClientID()
	clock := txn.Store().GetClock(clientID)

	var origin *block.ID
	var left *block.Item
	if existing, ok := m.branch.Map[key]; ok {
		left = existing
		lid := left.LastID()
		origin = &lid
	}

	keyCopy := key
	item := &block.Item{
		ID:        block.ID{Client: clientID, Clock: clock},
		Len:       1,
		Origin:    origin,
		Left:      left,
		Content:   block.Content{Kind: block.KindType, Branch: inner},
		Parent:    block.Parent{Kind: block.ParentBranch, Branch: m.branch},
		ParentSub: &keyCopy,
		Flags:     block.FlagCountable,
	}

	txn.Store().PushBlock(item)
	if dropped := item.Integrate(txn, 0); dropped {
		txn.Delete(item)
	}
}

// extractValue unpacks a Content into the user-visible value.
//
// For KindType the returned wrapper is the right concrete type
// based on Branch.TypeRef — *Map for TypeRefMap, *Array for
// TypeRefArray, *Text for TypeRefText. Returns nil for branch
// types we have not implemented yet (Xml*, Doc).
func extractValue(c block.Content) any {
	switch c.Kind {
	case block.KindAny:
		if len(c.Anys) > 0 {
			return c.Anys[0]
		}
	case block.KindString:
		return c.Str
	case block.KindEmbed:
		if len(c.Anys) > 0 {
			return c.Anys[0]
		}
	case block.KindBinary:
		return c.Bytes
	case block.KindType:
		if c.Branch == nil {
			return nil
		}
		switch c.Branch.TypeRef {
		case block.TypeRefMap:
			return NewMap(c.Branch)
		case block.TypeRefArray:
			return NewArray(c.Branch)
		case block.TypeRefText:
			return NewText(c.Branch)
		}
	}
	return nil
}
