package block

// maxTextMarkers is yjs's maxSearchMarker (AbstractType.js:27).
const maxTextMarkers = 80

// TextMarker is yjs's ArraySearchMarker (AbstractType.js:38-49): an item of
// a text and the index at which it starts. P may point at a format marker or
// a tombstone (findMarker can stop on one); Update moves it to the live
// content before it.
type TextMarker struct {
	P         *Item
	Index     int64
	Timestamp uint64
}

// TextMarkers is yjs's _searchMarker list for a text whose root was typed
// after formatted text arrived (Branch.LateText). yjs turns a type's
// markers off for good when a format marker integrates into it
// (ContentFormat.integrate), but Doc.get builds a fresh YText over a root
// that already holds format markers, with its markers on; until the next
// format marker integrates, insert, insertEmbed without attributes and
// delete start at a marker with only the attributes of the markers passed
// from there, so which markers exist decides where text goes. This list is
// a port of yjs's, step for step (findMarker, updateMarkerChanges, the
// mergeWith repoint, the clears), kept apart from MarkerList, which serves
// texts without format markers and arrays, where the result does not depend
// on the markers.
//
// The order of list is yjs's array order (a nearest-marker tie goes to the
// later entry), and Item's FlagMarked is yjs's Item.marker: set on the item
// a marker points at, cleared when the marker moves off it, and left as it
// is when the list is cleared or a merge moves a marker, as yjs leaves it;
// a stale flag then decides which markers Update drops. The timestamps are
// per list (yjs's counter is global; only their order within one list is
// compared).
//
// Mutated only under a write transaction, like MarkerList.
type TextMarkers struct {
	list  []*TextMarker
	clock uint64
}

// Len returns the number of markers.
func (m *TextMarkers) Len() int {
	if m == nil {
		return 0
	}
	return len(m.list)
}

// At returns the i-th marker in yjs's array order.
func (m *TextMarkers) At(i int) *TextMarker { return m.list[i] }

// Clear empties the list and leaves the items' marker flags (yjs
// _searchMarker.length = 0, in AbstractType._callObserver for a remote
// transaction and in UndoManager popStackItem).
func (m *TextMarkers) Clear() {
	if m == nil {
		return
	}
	clear(m.list)
	m.list = m.list[:0]
}

func (m *TextMarkers) tick() uint64 {
	m.clock++
	return m.clock
}

func absDiff64(a, b int64) int64 {
	if a < b {
		return b - a
	}
	return a - b
}

// Find is yjs findMarker (AbstractType.js:102-173): the marker to start a
// lookup of index from, moved or created for it, or nil when the lookup
// starts at the text's first item (an empty text, or index 0).
func (m *TextMarkers) Find(br *Branch, index uint64) *TextMarker {
	if br.Start == nil || index == 0 {
		return nil
	}
	idx := int64(index)
	// The nearest marker; on a tie, the later one (yjs reduce keeps a only
	// when it is strictly nearer).
	var marker *TextMarker
	for i, b := range m.list {
		if i == 0 || absDiff64(idx, marker.Index) >= absDiff64(idx, b.Index) {
			marker = b
		}
	}
	p := br.Start
	pindex := int64(0)
	if marker != nil {
		p = marker.P
		pindex = marker.Index
		marker.Timestamp = m.tick() // refreshMarkerTimestamp
	}
	// Iterate right while possible.
	for p.Right != nil && pindex < idx {
		if !p.IsDeleted() && p.IsCountable() {
			if idx < pindex+int64(p.Len) {
				break
			}
			pindex += int64(p.Len)
		}
		p = p.Right
	}
	// Iterate left if the marker was past index.
	for p.Left != nil && pindex > idx {
		p = p.Left
		if !p.IsDeleted() && p.IsCountable() {
			pindex -= int64(p.Len)
		}
	}
	// Step back over items p could merge into, which would leave the
	// marker on an item that is gone: the left neighbour of the same
	// client whose clocks run on into p's, deleted or not, format or not.
	for p.Left != nil && p.Left.ID.Client == p.ID.Client && p.Left.ID.Clock+p.Left.Len == p.ID.Clock {
		p = p.Left
		if !p.IsDeleted() && p.IsCountable() {
			pindex -= int64(p.Len)
		}
	}
	// |marker.index - pindex| < length / maxSearchMarker, in integers.
	if marker != nil && absDiff64(marker.Index, pindex)*maxTextMarkers < int64(br.BlockLen) {
		m.overwrite(marker, p, pindex)
		return marker
	}
	return m.mark(p, pindex)
}

// mark is yjs markPosition: a new marker, or the least recently used one
// moved, once the list holds maxTextMarkers.
func (m *TextMarkers) mark(p *Item, index int64) *TextMarker {
	if len(m.list) >= maxTextMarkers {
		lru := m.list[0]
		for _, b := range m.list[1:] {
			if lru.Timestamp >= b.Timestamp {
				lru = b
			}
		}
		m.overwrite(lru, p, index)
		return lru
	}
	p.SetMarked(true)
	mk := &TextMarker{P: p, Index: index, Timestamp: m.tick()}
	m.list = append(m.list, mk)
	return mk
}

// overwrite is yjs overwriteMarker.
func (m *TextMarkers) overwrite(mk *TextMarker, p *Item, index int64) {
	mk.P.SetMarked(false)
	mk.P = p
	p.SetMarked(true)
	mk.Index = index
	mk.Timestamp = m.tick()
}

// Update is yjs updateMarkerChanges (AbstractType.js:184-215), called after
// an insert of delta units at index (delta > 0) or a delete (delta < 0),
// with the index the cursor was at. On an insert each marker first moves
// left to live content, and a marker that finds none, or finds an item
// another marker already holds, is dropped.
func (m *TextMarkers) Update(index uint64, delta int64) {
	if m == nil {
		return
	}
	idx := int64(index)
	for i := len(m.list) - 1; i >= 0; i-- {
		mk := m.list[i]
		if delta > 0 {
			p := mk.P
			p.SetMarked(false)
			for p != nil && (p.IsDeleted() || !p.IsCountable()) {
				p = p.Left
				if p != nil && !p.IsDeleted() && p.IsCountable() {
					mk.Index -= int64(p.Len)
				}
			}
			if p == nil || p.IsMarked() {
				m.list = append(m.list[:i], m.list[i+1:]...)
				continue
			}
			mk.P = p
			p.SetMarked(true)
		}
		if idx < mk.Index || (delta > 0 && idx == mk.Index) {
			mk.Index = max(idx, mk.Index+delta)
		}
	}
}

// OnMerge is the repoint in yjs Item.mergeWith (Item.js:585-597): right
// merged into left, whose length before the merge was leftLen, so a marker
// on right now points at left, its index less leftLen when left is live
// content. The marker flags stay as they were.
func (m *TextMarkers) OnMerge(right, left *Item, leftLen uint64) {
	if m == nil {
		return
	}
	for _, mk := range m.list {
		if mk.P == right {
			mk.P = left
			if !left.IsDeleted() && left.IsCountable() {
				mk.Index -= int64(leftLen)
			}
		}
	}
}
