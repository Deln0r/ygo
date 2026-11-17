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
// holds a few contents with orders, as a live edit does. The set lives as
// long as the document and keeps at most maxOrderShapes shapes; past that,
// new shapes keep the orders they came with. Orders never change once
// built, so replacing one with another of the same shape changes nothing
// a reader sees.
type OrderSet struct {
	shapes  map[string]*KeyOrder
	singles map[*KeyOrder]*Orders
	key     []byte
	// What the last call took and gave: the contents of one update
	// mostly share their orders.
	lastIn, lastOut           *Orders
	lastOrderIn, lastOrderOut *KeyOrder
}

// Len returns the number of shapes s keeps.
func (s *OrderSet) Len() int { return len(s.shapes) }

// maxOrderShapes bounds the shapes an OrderSet keeps: a document's
// distinct shapes, which a peer chooses.
const maxOrderShapes = 4096

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
		out = s.single(s.order(os.List[0]), os)
	} else {
		var list []*KeyOrder
		for i, o := range os.List {
			c := s.order(o)
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
	if os.List[0] != o {
		os = &Orders{List: []*KeyOrder{o}}
	}
	if len(s.singles) < maxOrderShapes {
		if s.singles == nil {
			s.singles = map[*KeyOrder]*Orders{}
		}
		s.singles[o] = os
	}
	return os
}

// order returns the set's order of o's shape, its children first.
func (s *OrderSet) order(o *KeyOrder) *KeyOrder {
	if o == nil {
		return nil
	}
	if o == s.lastOrderIn {
		return s.lastOrderOut
	}
	var subs []*KeyOrder
	for i, sub := range o.Sub {
		c := s.order(sub)
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
	c, ok := s.shapes[string(k)]
	if !ok {
		c = o
		if subs != nil {
			c = &KeyOrder{Keys: o.Keys, Sub: subs}
		}
		if len(s.shapes) < maxOrderShapes {
			if s.shapes == nil {
				s.shapes = map[string]*KeyOrder{}
			}
			s.shapes[string(k)] = c
		}
	}
	s.lastOrderIn, s.lastOrderOut = o, c
	return c
}
