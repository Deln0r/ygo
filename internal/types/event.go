package types

import (
	"sort"

	"github.com/Deln0r/ygo/internal/block"
	"github.com/Deln0r/ygo/internal/doc"
)

func init() {
	// Bridge the doc -> types direction without an import cycle: doc
	// invokes this hook at Commit to dispatch shared-type events.
	doc.TypeEventHook = dispatchEvents
}

// KeyChange describes one map key's change within a transaction: the
// kind of change and the value the key held before it. Mirrors yjs's
// YMapEvent change entry ({ action, oldValue }).
type KeyChange struct {
	// Action is "add", "update", or "delete".
	Action string
	// OldValue is the value the key held before this transaction; nil
	// for an "add".
	OldValue any
}

// MapEvent is delivered to Map observers after a transaction that
// changed the map. Mirrors yjs YMapEvent.
type MapEvent struct {
	// Target is the map that changed.
	Target *Map
	// Path is the location of Target relative to the type whose
	// ObserveDeep callback is firing; empty for a shallow Observe and
	// for a deep observer on Target itself.
	Path []any
	// Keys maps each changed key to how it changed. Only keys whose
	// observable value actually changed appear (a key added and
	// removed in the same transaction is omitted, matching yjs).
	Keys map[string]KeyChange
}

// KeysChanged returns the set of keys that changed, for callers that
// only need the names.
func (e *MapEvent) KeysChanged() []string {
	out := make([]string, 0, len(e.Keys))
	for k := range e.Keys {
		out = append(out, k)
	}
	return out
}

// Observe registers fn to run after every transaction that changes
// this map, with a MapEvent describing the changed keys. Returns an
// unsubscribe function. Observers fire on local edits and on remote
// updates applied via ApplyUpdate.
//
// Mirrors yjs ymap.observe. The callback runs while the doc write
// lock is held; it must not start a new transaction on the same doc.
func (m *Map) Observe(fn func(*MapEvent)) func() {
	adapter := func(ev any) {
		if e, ok := ev.(*MapEvent); ok {
			fn(e)
		}
	}
	m.branch.Observers = append(m.branch.Observers, adapter)
	idx := len(m.branch.Observers) - 1
	return func() {
		// Detach by clearing the slot; keep indices stable so a later
		// unsubscribe does not target the wrong observer.
		if idx < len(m.branch.Observers) {
			m.branch.Observers[idx] = nil
		}
	}
}

// ObserveDeep registers fn to run after any transaction that changes
// this map OR any shared type nested under it, with the list of events
// (each carrying its Path relative to this map). Returns an
// unsubscribe function. Mirrors yjs ymap.observeDeep.
func (m *Map) ObserveDeep(fn func([]any)) func() {
	return observeDeep(m.branch, fn)
}

// dispatchEvents is installed as doc.TypeEventHook. It fires observers the
// way yjs cleanupTransactions does: types in the order they were first
// changed, deleted types skipped, then every deep observer once per
// transaction with all the events under it.
//
// Routing is by which kind of change was recorded (keyed vs positional),
// which is reliable even for root branches whose TypeRef defaults to zero.
func dispatchEvents(t *doc.TransactionMut) {
	// Events per type, for the type itself and each of its ancestors, in
	// the order the types were first reached: yjs changedParentTypes. An
	// event is recorded here before its shallow observers run, as yjs
	// callTypeObservers does.
	var parents []*block.Branch
	under := map[*block.Branch][]any{}

	for _, br := range t.ChangedTypes() {
		// A type deleted later in this transaction fires nothing.
		if br.Item != nil && br.Item.IsDeleted() {
			continue
		}
		var ev any
		if keys := t.ChangedKeys(br); len(keys) > 0 {
			ev = buildMapEvent(t, br, keys)
		} else if t.PositionalChanged(br) {
			// Positional change: Text (tagged TypeRefText by NewText) vs
			// Array (the default). Root branches otherwise share TypeRef
			// zero, so we cannot rely on it for arrays; the Text tag is the
			// one reliable discriminator.
			if br.TypeRef == block.TypeRefText {
				ev = &TextEvent{Target: &Text{branch: br}, Delta: buildTextDelta(t, br)}
			} else {
				ev = &ArrayEvent{Target: &Array{branch: br}, Delta: buildPositionalDelta(t, br)}
			}
		}
		if ev == nil {
			continue
		}
		for anc := br; anc != nil; anc = ancestorBranch(anc) {
			if _, seen := under[anc]; !seen {
				parents = append(parents, anc)
			}
			under[anc] = append(under[anc], ev)
		}
		for _, o := range br.Observers {
			if o != nil {
				o(ev)
			}
		}
	}

	// Deep observers run after every shallow one, each once, with the
	// events of its own type and everything under it: shallower paths
	// first, events of the same depth in the order they were recorded.
	// Each type gets its own copy of an event with the path relative to
	// that type, so a callback that keeps an event keeps the path it was
	// given (yjs shares one event object and moves its path, #768).
	// Which types get a deep call is settled before the first deep callback
	// runs, as yjs queues them: a type that had no deep observer at that
	// point gets no deep call in this transaction, even if a deep callback
	// registers one on it.
	var deepTargets []*block.Branch
	for _, anc := range parents {
		if hasDeepObserver(anc) && (anc.Item == nil || !anc.Item.IsDeleted()) {
			deepTargets = append(deepTargets, anc)
		}
	}
	for _, anc := range deepTargets {
		type pathed struct {
			ev    any
			depth int
		}
		batch := make([]pathed, 0, len(under[anc]))
		for _, ev := range under[anc] {
			target := eventBranch(ev)
			if target == nil || (target.Item != nil && target.Item.IsDeleted()) {
				continue
			}
			path, ok := pathBetween(anc, target)
			if !ok {
				continue
			}
			batch = append(batch, pathed{ev: withPath(ev, path), depth: len(path)})
		}
		sort.SliceStable(batch, func(i, j int) bool { return batch[i].depth < batch[j].depth })
		events := make([]any, len(batch))
		for i, p := range batch {
			events[i] = p.ev
		}
		for _, o := range anc.DeepObservers {
			if o != nil {
				o(events)
			}
		}
	}
}

// hasDeepObserver reports whether br has a live deep observer. An
// unsubscribed observer leaves a nil slot behind.
func hasDeepObserver(br *block.Branch) bool {
	for _, o := range br.DeepObservers {
		if o != nil {
			return true
		}
	}
	return false
}

// eventBranch returns the branch an event is about.
func eventBranch(ev any) *block.Branch {
	switch e := ev.(type) {
	case *MapEvent:
		return e.Target.branch
	case *ArrayEvent:
		return e.Target.branch
	case *TextEvent:
		return e.Target.branch
	}
	return nil
}

// ancestorBranch returns the branch that directly contains br (the
// parent of br's owning item), or nil if br is a root.
func ancestorBranch(br *block.Branch) *block.Branch {
	if br == nil || br.Item == nil {
		return nil
	}
	return br.Item.Parent.Branch
}

// pathBetween returns the path of segments (map keys and array
// indices) from ancestor down to target, or ok=false when target is
// not nested under ancestor.
func pathBetween(ancestor, target *block.Branch) ([]any, bool) {
	var segs []any
	cur := target
	for cur != ancestor {
		item := cur.Item
		if item == nil {
			return nil, false // reached a root without hitting ancestor
		}
		if item.ParentSub != nil {
			segs = append(segs, *item.ParentSub)
		} else {
			segs = append(segs, itemIndex(item))
		}
		cur = item.Parent.Branch
		if cur == nil {
			return nil, false
		}
	}
	// segs is target..ancestor; reverse to ancestor..target.
	for i, j := 0, len(segs)-1; i < j; i, j = i+1, j-1 {
		segs[i], segs[j] = segs[j], segs[i]
	}
	return segs, true
}

// itemIndex returns the positional index of item within its parent's
// live element sequence (the path segment for an array-nested type).
func itemIndex(item *block.Item) int {
	idx := 0
	parent := item.Parent.Branch
	if parent == nil {
		return 0
	}
	for it := parent.Start; it != nil && it != item; it = it.Right {
		if !it.IsDeleted() && it.IsCountable() {
			idx += int(it.Len)
		}
	}
	return idx
}

// withPath returns a copy of ev carrying path, the location of its target
// relative to the deep observer it is delivered to.
func withPath(ev any, path []any) any {
	switch e := ev.(type) {
	case *MapEvent:
		c := *e
		c.Path = path
		return &c
	case *ArrayEvent:
		c := *e
		c.Path = path
		return &c
	case *TextEvent:
		c := *e
		c.Path = path
		return &c
	}
	return ev
}

// adds reports whether item was created during this transaction (the
// y-protocols adds() predicate: clock >= beforeState).
func adds(t *doc.TransactionMut, it *block.Item) bool {
	return it != nil && it.ID.Clock >= t.BeforeState()[it.ID.Client]
}

// deletes reports whether item was tombstoned during this transaction.
func deletes(t *doc.TransactionMut, it *block.Item) bool {
	return it != nil && t.DeletedThisTxn(it.ID)
}

// contentValues returns each element value of an item's content, the
// equivalent of yjs item.content.getContent(). A multi-value
// ContentAny item (a packed range insert) yields all its values; other
// content kinds yield a single value.
func contentValues(c block.Content) []any {
	if c.Kind == block.KindAny {
		return append([]any(nil), c.Anys...)
	}
	return []any{extractValue(c)}
}

// buildMapEvent builds a MapEvent for a changed map branch following
// yjs YMapEvent.keys semantics. The caller fires it to observers.
func buildMapEvent(t *doc.TransactionMut, br *block.Branch, changed map[string]struct{}) *MapEvent {
	before := t.BeforeState()
	adds := func(it *block.Item) bool {
		return it != nil && it.ID.Clock >= before[it.ID.Client]
	}
	deletes := func(it *block.Item) bool {
		return it != nil && t.DeletedThisTxn(it.ID)
	}

	keys := make(map[string]KeyChange, len(changed))
	for key := range changed {
		item := br.Map[key] // current winner (may be deleted)
		if item == nil {
			continue
		}
		var change KeyChange
		var skip bool
		if adds(item) {
			prev := item.Left
			for prev != nil && adds(prev) {
				prev = prev.Left
			}
			if deletes(item) {
				if prev != nil && deletes(prev) {
					change = KeyChange{Action: "delete", OldValue: extractValue(prev.Content)}
				} else {
					skip = true // added and removed this txn: net no-op
				}
			} else {
				if prev != nil && deletes(prev) {
					change = KeyChange{Action: "update", OldValue: extractValue(prev.Content)}
				} else {
					change = KeyChange{Action: "add"}
				}
			}
		} else {
			if deletes(item) {
				change = KeyChange{Action: "delete", OldValue: extractValue(item.Content)}
			} else {
				skip = true // no observable change
			}
		}
		if !skip {
			keys[key] = change
		}
	}
	// Return even when keys ends up empty (e.g. a key added and removed
	// in the same transaction nets to no-op): yjs still delivers the
	// event, with an empty changes.keys, because the type was touched.
	return &MapEvent{Target: &Map{branch: br}, Keys: keys}
}

// ArrayDeltaOp is one operation of an array change delta. Exactly one
// of the three is set: Insert (the inserted values), Delete (a count
// of removed elements), or Retain (a count of unchanged elements). The
// text DeltaOp (text_format.go) is the string-valued analogue for Text.
type ArrayDeltaOp struct {
	Insert []any
	Delete int
	Retain int
}

// ArrayEvent is delivered to Array observers (Array.Observe) after a
// transaction that changed the array. Delta is the Quill-style change
// description. Mirrors yjs YArrayEvent.
type ArrayEvent struct {
	Target *Array
	// Path locates Target relative to the ObserveDeep type firing;
	// empty for shallow Observe.
	Path  []any
	Delta []ArrayDeltaOp
}

// Observe registers fn to run after every transaction that changes
// this array, with an ArrayEvent carrying the change delta. Returns an
// unsubscribe function. Fires on local edits and remote updates.
//
// Mirrors yjs yarray.observe. The callback runs while the doc write
// lock is held; it must not start a new transaction on the same doc.
func (a *Array) Observe(fn func(*ArrayEvent)) func() {
	adapter := func(ev any) {
		if e, ok := ev.(*ArrayEvent); ok {
			fn(e)
		}
	}
	a.branch.Observers = append(a.branch.Observers, adapter)
	idx := len(a.branch.Observers) - 1
	return func() {
		if idx < len(a.branch.Observers) {
			a.branch.Observers[idx] = nil
		}
	}
}

// op kinds for the delta builder.
const (
	opNone = iota
	opInsert
	opDelete
	opRetain
)

// buildPositionalDelta walks a branch's positional item list and
// builds the array change delta following yjs YEvent.changes: items
// deleted this transaction (and not also added) become delete ops,
// items added this transaction become insert ops, surviving
// pre-existing items become retain ops. A trailing retain is dropped
// (a delta ending in retain is meaningless).
func buildPositionalDelta(t *doc.TransactionMut, br *block.Branch) []ArrayDeltaOp {
	var delta []ArrayDeltaOp
	var cur ArrayDeltaOp
	kind := opNone
	pack := func() {
		if kind != opNone {
			delta = append(delta, cur)
			cur = ArrayDeltaOp{}
			kind = opNone
		}
	}
	for it := br.Start; it != nil; it = it.Right {
		switch {
		case it.IsDeleted():
			if deletes(t, it) && !adds(t, it) {
				if kind != opDelete {
					pack()
					kind = opDelete
				}
				cur.Delete += int(it.Len)
			}
		case adds(t, it):
			if kind != opInsert {
				pack()
				cur = ArrayDeltaOp{Insert: []any{}}
				kind = opInsert
			}
			cur.Insert = append(cur.Insert, contentValues(it.Content)...)
		default:
			if kind != opRetain {
				pack()
				kind = opRetain
			}
			cur.Retain += int(it.Len)
		}
	}
	// Drop a trailing retain; pack any trailing insert/delete.
	if kind != opNone && kind != opRetain {
		pack()
	}
	return delta
}

// ObserveDeep registers fn to run after any transaction that changes
// this array OR any shared type nested under it, with the list of
// events (each carrying its Path relative to this array). Returns an
// unsubscribe function. Mirrors yjs yarray.observeDeep.
func (a *Array) ObserveDeep(fn func([]any)) func() {
	return observeDeep(a.branch, fn)
}

// TextEvent is delivered to Text observers (Text.Observe) after a
// transaction that changed the text. Delta is the Quill-style,
// formatting-aware change description (the same DeltaOp shape ToDelta
// produces). Mirrors yjs YTextEvent.
type TextEvent struct {
	Target *Text
	// Path locates Target relative to the ObserveDeep type firing;
	// empty for shallow Observe.
	Path  []any
	Delta []DeltaOp
}

// Observe registers fn to run after every transaction that changes
// this text, with a TextEvent carrying the formatting-aware change
// delta. Returns an unsubscribe function. Fires on local edits and
// remote updates.
//
// Mirrors yjs ytext.observe. The callback runs while the doc write
// lock is held; it must not start a new transaction on the same doc.
func (t *Text) Observe(fn func(*TextEvent)) func() {
	adapter := func(ev any) {
		if e, ok := ev.(*TextEvent); ok {
			fn(e)
		}
	}
	t.branch.Observers = append(t.branch.Observers, adapter)
	idx := len(t.branch.Observers) - 1
	return func() {
		if idx < len(t.branch.Observers) {
			t.branch.Observers[idx] = nil
		}
	}
}

// observeDeep registers a deep observer on a branch and returns an
// unsubscribe. Shared by Map.ObserveDeep / Array.ObserveDeep.
func observeDeep(br *block.Branch, fn func([]any)) func() {
	br.DeepObservers = append(br.DeepObservers, fn)
	idx := len(br.DeepObservers) - 1
	return func() {
		if idx < len(br.DeepObservers) {
			br.DeepObservers[idx] = nil
		}
	}
}

// buildTextDelta is a faithful port of yjs YTextEvent.delta: it walks
// the text's items tracking the formatting active at the cursor
// (currentAttrs), and emits insert (string or embed) / delete / retain
// ops, where retain ops carry the attribute changes a format marker
// introduced. Trailing retains without attribute changes are dropped.
func buildTextDelta(txn *doc.TransactionMut, br *block.Branch) []DeltaOp {
	var delta []DeltaOp
	currentAttrs := Attrs{}
	oldAttrs := Attrs{}
	attributes := Attrs{} // retain-op attribute diff
	action := ""          // "insert" | "delete" | "retain" | ""
	insertStr := ""
	var insertEmbed any
	insertIsEmbed := false
	var retain, deleteLen uint64

	addOp := func() {
		if action == "" {
			return
		}
		switch action {
		case "delete":
			if deleteLen > 0 {
				delta = append(delta, DeltaOp{Delete: deleteLen})
			}
			deleteLen = 0
		case "insert":
			if insertIsEmbed {
				delta = append(delta, DeltaOp{Embed: insertEmbed, Attributes: nonNullAttrs(currentAttrs)})
			} else if insertStr != "" {
				delta = append(delta, DeltaOp{Insert: insertStr, Attributes: nonNullAttrs(currentAttrs)})
			}
			insertStr = ""
			insertEmbed = nil
			insertIsEmbed = false
		case "retain":
			if retain > 0 {
				op := DeltaOp{Retain: retain}
				if len(attributes) > 0 {
					op.Attributes = copyAttrs(attributes)
				}
				delta = append(delta, op)
			}
			retain = 0
		}
		action = ""
	}

	for it := br.Start; it != nil; it = it.Right {
		switch it.Content.Kind {
		case block.KindEmbed, block.KindType, block.KindDoc:
			if adds(txn, it) {
				if !deletes(txn, it) {
					addOp()
					action = "insert"
					insertIsEmbed = true
					insertEmbed = contentValues(it.Content)[0]
					addOp()
				}
			} else if deletes(txn, it) {
				if action != "delete" {
					addOp()
					action = "delete"
				}
				deleteLen++
			} else if !it.IsDeleted() {
				if action != "retain" {
					addOp()
					action = "retain"
				}
				retain++
			}
		case block.KindString:
			if adds(txn, it) {
				if !deletes(txn, it) {
					if action != "insert" {
						addOp()
						action = "insert"
					}
					insertStr += it.Content.Str
				}
			} else if deletes(txn, it) {
				if action != "delete" {
					addOp()
					action = "delete"
				}
				deleteLen += it.Len
			} else if !it.IsDeleted() {
				if action != "retain" {
					addOp()
					action = "retain"
				}
				retain += it.Len
			}
		case block.KindFormat:
			key := it.Content.FormatKey
			var value any
			if len(it.Content.Anys) > 0 {
				value = it.Content.Anys[0]
			}
			if adds(txn, it) {
				if !deletes(txn, it) {
					if !attrValuesEqual(currentAttrs[key], value) {
						if action == "retain" {
							addOp()
						}
						if attrValuesEqual(value, oldAttrs[key]) {
							delete(attributes, key)
						} else {
							attributes[key] = value
						}
					}
				}
			} else if deletes(txn, it) {
				oldAttrs[key] = value
				if !attrValuesEqual(currentAttrs[key], value) {
					if action == "retain" {
						addOp()
					}
					attributes[key] = currentAttrs[key]
				}
			} else if !it.IsDeleted() {
				oldAttrs[key] = value
				if attr, ok := attributes[key]; ok {
					if !attrValuesEqual(attr, value) {
						if action == "retain" {
							addOp()
						}
						if value == nil {
							delete(attributes, key)
						} else {
							attributes[key] = value
						}
					}
				}
			}
			if !it.IsDeleted() {
				if action == "insert" {
					addOp()
				}
				updateCurrentAttrs(currentAttrs, key, value)
			}
		}
	}
	addOp()
	// Drop trailing retains that carry no attribute change.
	for len(delta) > 0 {
		last := delta[len(delta)-1]
		if last.Retain > 0 && last.Insert == "" && last.Embed == nil && last.Delete == 0 && len(last.Attributes) == 0 {
			delta = delta[:len(delta)-1]
		} else {
			break
		}
	}
	return delta
}

// nonNullAttrs copies attrs, dropping keys whose value is nil (a nil
// value in currentAttributes means the attribute is cleared, which an
// insert op should not carry). Returns nil for an empty result.
func nonNullAttrs(attrs Attrs) Attrs {
	var out Attrs
	for k, v := range attrs {
		if v != nil {
			if out == nil {
				out = Attrs{}
			}
			out[k] = v
		}
	}
	return out
}
