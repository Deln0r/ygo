package encoding

import (
	"encoding/binary"
	"sync"
	"unsafe"

	"github.com/Deln0r/ygo/internal/block"
)

// Decoded objects of one shape (the same keys in the same order, with the
// same orders inside) share one KeyOrder, and a one-element Content one
// Orders, so a document holding many records written by JavaScript, whose
// keys are rarely in sorted order, pays for each shape once rather than
// for each object. Orders never change once built, so sharing them across
// documents is safe. The table stops growing at maxInternedOrders shapes;
// past that, orders are built as they come.
const maxInternedOrders = 4096

var interned struct {
	mu      sync.Mutex
	orders  map[string]*block.KeyOrder
	singles map[*block.KeyOrder]*block.Orders
}

// internOrder returns the shared KeyOrder for keys and subs, building it
// with a copy of keys when the shape is new. keys and subs are not kept.
func internOrder(keys []string, subs []*block.KeyOrder) *block.KeyOrder {
	var small [256]byte
	k := small[:0]
	for _, s := range keys {
		k = binary.AppendUvarint(k, uint64(len(s)))
		k = append(k, s...)
	}
	k = append(k, 0xff)
	for _, s := range subs {
		k = binary.LittleEndian.AppendUint64(k, uint64(uintptr(unsafe.Pointer(s))))
	}
	interned.mu.Lock()
	defer interned.mu.Unlock()
	if o, ok := interned.orders[string(k)]; ok {
		return o
	}
	o := &block.KeyOrder{Keys: append([]string(nil), keys...)}
	if subs != nil {
		o.Sub = append([]*block.KeyOrder(nil), subs...)
	}
	if len(interned.orders) < maxInternedOrders {
		if interned.orders == nil {
			interned.orders = map[string]*block.KeyOrder{}
		}
		interned.orders[string(k)] = o
	}
	return o
}

// singleOrders returns the shared Orders of a one-element Content whose
// element has the order o.
func singleOrders(o *block.KeyOrder) *block.Orders {
	if o == nil {
		return nil
	}
	interned.mu.Lock()
	defer interned.mu.Unlock()
	if os, ok := interned.singles[o]; ok {
		return os
	}
	os := &block.Orders{List: []*block.KeyOrder{o}}
	if len(interned.singles) < maxInternedOrders {
		if interned.singles == nil {
			interned.singles = map[*block.KeyOrder]*block.Orders{}
		}
		interned.singles[o] = os
	}
	return os
}
