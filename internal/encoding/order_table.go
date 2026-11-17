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
// The orders returned last sit in small caches: eight for objects, by
// their number of keys and first key (objectSlot), one for arrays, one
// for Orders. Records of one
// shape come one after another, and comparing with these skips building a
// key. An order leaves its cache only for the maps, made then, so every
// order built is in a cache or a map, and an update of a few records
// makes no map at all.
//
// A map key holds the addresses of the orders inside. They are orders of
// the same table, alive until it is dropped, so an address is not reused
// while it is a key.
type orderTable struct {
	lastObject [8]*block.KeyOrder
	lastArray  *block.KeyOrder
	lastSingle *block.Orders
	orders     map[string]*block.KeyOrder
	singles    map[*block.KeyOrder]*block.Orders
	key        []byte // scratch for the key being looked up
}

// object returns the shared order of an object whose keys, in JavaScript
// enumeration order, are keys, with subs the orders of their values, or
// nil subs when none has one. keys and subs are not kept. With clone the
// keys of a new order are copied, for keys that point into a larger
// string that the order would otherwise keep alive.
func (t *orderTable) object(keys []string, subs []*block.KeyOrder, clone bool) *block.KeyOrder {
	slot := &t.lastObject[objectSlot(keys)]
	if o := *slot; o != nil && sameObjectShape(o, keys, subs) {
		return o
	}
	var o *block.KeyOrder
	if t.orders != nil {
		o = t.orders[string(t.objectKey(keys, subs))]
	}
	if o == nil {
		o = &block.KeyOrder{Keys: make([]string, len(keys))}
		for i, s := range keys {
			if clone {
				s = strings.Clone(s)
			}
			o.Keys[i] = s
		}
		if subs != nil {
			o.Sub = append([]*block.KeyOrder(nil), subs...)
		}
	}
	t.evict(*slot)
	*slot = o
	return o
}

// objectSlot spreads objects over the cache by their number of keys and
// the first byte of the first: a record and the object inside it often
// have as many keys.
func objectSlot(keys []string) int {
	h := len(keys) * 31
	if len(keys) > 0 && keys[0] != "" {
		h += int(keys[0][0])
	}
	return h & 7
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
	var o *block.KeyOrder
	if t.orders != nil {
		o = t.orders[string(t.arrayKey(subs))]
	}
	if o == nil {
		o = &block.KeyOrder{Sub: append([]*block.KeyOrder(nil), subs...)}
	}
	t.evict(t.lastArray)
	t.lastArray = o
	return o
}

// evict keeps o, leaving its cache, in the map.
func (t *orderTable) evict(o *block.KeyOrder) {
	if o == nil {
		return
	}
	var k []byte
	if o.Keys != nil {
		k = t.objectKey(o.Keys, o.Sub)
	} else {
		k = t.arrayKey(o.Sub)
	}
	if t.orders == nil {
		t.orders = map[string]*block.KeyOrder{}
	}
	if _, ok := t.orders[string(k)]; !ok {
		t.orders[string(k)] = o
	}
}

// objectKey and arrayKey build the map key of a shape in t.key: a kind
// byte, then counted keys and the addresses of the orders inside.
func (t *orderTable) objectKey(keys []string, subs []*block.KeyOrder) []byte {
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
	return k
}

func (t *orderTable) arrayKey(subs []*block.KeyOrder) []byte {
	k := append(t.key[:0], 'a')
	k = binary.AppendUvarint(k, uint64(len(subs)))
	k = appendOrderAddrs(k, subs)
	t.key = k
	return k
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
	if os := t.lastSingle; os != nil && os.List[0] == o {
		return os
	}
	os := t.singles[o]
	if os == nil {
		os = &block.Orders{List: []*block.KeyOrder{o}}
	}
	if old := t.lastSingle; old != nil {
		if t.singles == nil {
			t.singles = map[*block.KeyOrder]*block.Orders{}
		}
		t.singles[old.List[0]] = old
	}
	t.lastSingle = os
	return os
}
