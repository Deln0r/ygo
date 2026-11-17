package encoding

import (
	"encoding/binary"
	"strings"
	"unsafe"

	"github.com/Deln0r/ygo/internal/block"
)

// orderTable shares the key orders built while one update is decoded.
// Objects of one shape (the same keys in the same order, with the same
// orders inside) get one KeyOrder, arrays whose elements have the same
// orders get one, and one-element contents with the same order one Orders.
// A document of records written by JavaScript, whose keys are rarely in
// sorted order, then pays for each shape once per update rather than once
// per record. The table lives as long as the decode, so nothing it holds
// outlives the documents that use the orders, and no lock is taken. Orders
// never change once built, so sharing them is safe.
//
// A table key holds the addresses of the orders inside. They are entries
// of the same table, alive until it is dropped, so an address is not
// reused while it is a key.
type orderTable struct {
	orders  map[string]*block.KeyOrder
	singles map[*block.KeyOrder]*block.Orders
	key     []byte // scratch for the key being looked up
	// The object order returned last for each number of keys (mod 8),
	// and the array order returned last: records of one shape come one
	// after another, and comparing with these skips building a key.
	lastObject [8]*block.KeyOrder
	lastArray  *block.KeyOrder
}

// object returns the shared order of an object whose keys, in JavaScript
// enumeration order, are keys, with subs the orders of their values, or
// nil subs when none has one. keys and subs are not kept. With clone the
// keys of a new order are copied, for keys that point into a larger
// string that the order would otherwise keep alive.
func (t *orderTable) object(keys []string, subs []*block.KeyOrder, clone bool) *block.KeyOrder {
	slot := &t.lastObject[len(keys)&7]
	if o := *slot; o != nil && sameObjectShape(o, keys, subs) {
		return o
	}
	o := t.lookupObject(keys, subs, clone)
	*slot = o
	return o
}

func sameObjectShape(o *block.KeyOrder, keys []string, subs []*block.KeyOrder) bool {
	if len(o.Keys) != len(keys) || (o.Sub == nil) != (subs == nil) {
		return false
	}
	for i, k := range keys {
		if o.Keys[i] != k {
			return false
		}
	}
	for i, s := range subs {
		if o.Sub[i] != s {
			return false
		}
	}
	return true
}

func (t *orderTable) lookupObject(keys []string, subs []*block.KeyOrder, clone bool) *block.KeyOrder {
	k := append(t.key[:0], 'o')
	k = binary.AppendUvarint(k, uint64(len(keys)))
	for _, s := range keys {
		k = binary.AppendUvarint(k, uint64(len(s)))
		k = append(k, s...)
	}
	if subs == nil {
		k = append(k, 0)
	} else {
		k = append(k, 1)
		k = appendOrderAddrs(k, subs)
	}
	t.key = k
	if o, ok := t.orders[string(k)]; ok {
		return o
	}
	o := &block.KeyOrder{Keys: make([]string, len(keys))}
	for i, s := range keys {
		if clone {
			s = strings.Clone(s)
		}
		o.Keys[i] = s
	}
	if subs != nil {
		o.Sub = append([]*block.KeyOrder(nil), subs...)
	}
	t.put(k, o)
	return o
}

// array returns the shared order of an array whose elements have the
// orders subs, of which at least one is set. Trailing elements without an
// order are left out (SubOrder reads past the end as none). subs is not
// kept.
func (t *orderTable) array(subs []*block.KeyOrder) *block.KeyOrder {
	for len(subs) > 0 && subs[len(subs)-1] == nil {
		subs = subs[:len(subs)-1]
	}
	if o := t.lastArray; o != nil && len(o.Sub) == len(subs) {
		same := true
		for i, s := range subs {
			if o.Sub[i] != s {
				same = false
				break
			}
		}
		if same {
			return o
		}
	}
	o := t.lookupArray(subs)
	t.lastArray = o
	return o
}

func (t *orderTable) lookupArray(subs []*block.KeyOrder) *block.KeyOrder {
	k := append(t.key[:0], 'a')
	k = binary.AppendUvarint(k, uint64(len(subs)))
	k = appendOrderAddrs(k, subs)
	t.key = k
	if o, ok := t.orders[string(k)]; ok {
		return o
	}
	o := &block.KeyOrder{Sub: append([]*block.KeyOrder(nil), subs...)}
	t.put(k, o)
	return o
}

func (t *orderTable) put(k []byte, o *block.KeyOrder) {
	if t.orders == nil {
		t.orders = map[string]*block.KeyOrder{}
	}
	t.orders[string(k)] = o
}

func appendOrderAddrs(k []byte, subs []*block.KeyOrder) []byte {
	for _, s := range subs {
		k = binary.LittleEndian.AppendUint64(k, uint64(uintptr(unsafe.Pointer(s))))
	}
	return k
}

// single returns the shared Orders of a one-element Content whose element
// has the order o, nil for none.
func (t *orderTable) single(o *block.KeyOrder) *block.Orders {
	if o == nil {
		return nil
	}
	if os, ok := t.singles[o]; ok {
		return os
	}
	os := &block.Orders{List: []*block.KeyOrder{o}}
	if t.singles == nil {
		t.singles = map[*block.KeyOrder]*block.Orders{}
	}
	t.singles[o] = os
	return os
}
