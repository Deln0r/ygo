package block

import (
	"encoding/binary"
	"unsafe"
)

// OrderSet holds one KeyOrder per shape for the contents of one document,
// so objects of one shape that arrived in updates of their own, as live
// edits do, share an order as those decoded from one update already do.
// Applying an update hands each content's Orders to Orders under the
// document's write lock, before the content is stored, when the update
// holds a few contents with orders, as a live edit does. Orders never
// change once built, so replacing one with another of the same shape
// changes nothing a reader sees.
//
// The set lives as long as the document, and a peer chooses the shapes,
// so what it keeps is bounded: at most maxOrderShapes entries, about
// maxOrderBytes of them, and no shape whose key would pass maxShapeKey
// bytes (an object of many keys rarely comes twice). An order the set
// cannot keep is returned as it came, children and all, so an update's
// own sharing stays.
type OrderSet struct {
	shapes  map[string]*KeyOrder
	singles map[*KeyOrder]*Orders
	bytes   int // what the entries hold, roughly
	key     []byte
	// What the last call took and gave: the contents of one update
	// mostly share their orders.
	lastIn, lastOut           *Orders
	lastOrderIn, lastOrderOut *KeyOrder
	lastKept                  bool
}

// Len returns the number of shapes s keeps.
func (s *OrderSet) Len() int { return len(s.shapes) }

// Bounds on what an OrderSet keeps (see OrderSet).
const (
	maxOrderShapes = 4096
	maxOrderBytes  = 1 << 20
	maxShapeKey    = 512
	// orderEntryCost is what an entry holds besides its key, roughly: the
	// KeyOrder and its slices, and the map's own share.
	orderEntryCost = 96
)

// room reports whether the set can keep another entry whose key has n
// bytes, and counts it if so.
func (s *OrderSet) room(n int) bool {
	cost := 2*n + orderEntryCost // the key, and the order's copy of it
	if len(s.shapes)+len(s.singles) >= maxOrderShapes || s.bytes+cost > maxOrderBytes {
		return false
	}
	s.bytes += cost
	return true
}

// Orders returns os with each order replaced by the set's one of its
// shape, os itself when nothing changes.
func (s *OrderSet) Orders(os *Orders) *Orders {
	if os == nil {
		return nil
	}
	if os == s.lastIn {
		return s.lastOut
	}
	out := os
	if len(os.List) == 1 {
		if c, kept := s.order(os.List[0]); kept {
			out = s.single(c, os)
		}
	} else {
		var list []*KeyOrder
		for i, o := range os.List {
			c, _ := s.order(o)
			if c != o && list == nil {
				list = append([]*KeyOrder(nil), os.List...)
			}
			if list != nil {
				list[i] = c
			}
		}
		if list != nil {
			out = &Orders{List: list}
		}
	}
	s.lastIn, s.lastOut = os, out
	return out
}

// single returns the set's one-element Orders for o, os (whose element
// has o's shape) when it has none yet and keeps it.
func (s *OrderSet) single(o *KeyOrder, os *Orders) *Orders {
	if have, ok := s.singles[o]; ok {
		return have
	}
	if !s.room(0) {
		return os
	}
	if os.List[0] != o {
		os = &Orders{List: []*KeyOrder{o}}
	}
	if s.singles == nil {
		s.singles = map[*KeyOrder]*Orders{}
	}
	s.singles[o] = os
	return os
}

// order returns the set's order of o's shape, its children first, and
// whether the set keeps it; o itself when it does not.
func (s *OrderSet) order(o *KeyOrder) (*KeyOrder, bool) {
	if o == nil {
		return nil, false
	}
	if o == s.lastOrderIn {
		return s.lastOrderOut, s.lastKept
	}
	size := 2 + 8*len(o.Sub)
	for _, key := range o.Keys {
		size += len(key) + 2
	}
	if size > maxShapeKey {
		return o, false // too large to keep; nor are the orders inside looked at
	}
	var subs []*KeyOrder
	for i, sub := range o.Sub {
		c, _ := s.order(sub)
		if c != sub && subs == nil {
			subs = append([]*KeyOrder(nil), o.Sub...)
		}
		if subs != nil {
			subs[i] = c
		}
	}
	cur := o.Sub
	if subs != nil {
		cur = subs
	}
	k := s.key[:0]
	if o.Keys == nil {
		k = append(k, 'a')
	} else {
		k = append(k, 'o')
		k = binary.AppendUvarint(k, uint64(len(o.Keys)))
		for _, key := range o.Keys {
			k = binary.AppendUvarint(k, uint64(len(key)))
			k = append(k, key...)
		}
	}
	k = binary.AppendUvarint(k, uint64(len(cur)))
	for _, sub := range cur {
		k = binary.LittleEndian.AppendUint64(k, uint64(uintptr(unsafe.Pointer(sub))))
	}
	s.key = k
	c, kept := s.shapes[string(k)]
	switch {
	case kept:
	case !s.room(len(k)):
		c = o // as it came, with the children it came with
	default:
		kept = true
		c = o
		if subs != nil {
			c = &KeyOrder{Keys: o.Keys, Sub: subs}
		}
		if s.shapes == nil {
			s.shapes = map[string]*KeyOrder{}
		}
		s.shapes[string(k)] = c
	}
	s.lastOrderIn, s.lastOrderOut, s.lastKept = o, c, kept
	return c, kept
}
