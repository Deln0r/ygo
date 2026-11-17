package block

// KeyOrder is the order to write the keys of the objects inside one
// decoded value (an Any element, an embed, a format value), as the peer
// that wrote them holds them: JavaScript enumeration order. A Go map has
// no order, so without it every object would be written with its keys
// sorted. A nil *KeyOrder means the fallback order is right for that value
// and everything inside it: array-index keys first, ascending, then the
// others sorted (anyval.SortedJSOrder). Orders are built when a value is
// decoded, only where they differ from that, and never change afterwards.
type KeyOrder struct {
	// Keys are an object's keys in the order to write them; nil for an
	// array.
	Keys []string
	// Sub holds the order of the value at Keys[i] for an object, or of
	// element i for an array; nil when no child needs one.
	Sub []*KeyOrder
}

// Orders holds the KeyOrder of each element of a Content's Anys, in step
// with them: len(List) == len(Anys). A Content without any keeps a nil
// pointer, so the field costs one word per item.
type Orders struct {
	List []*KeyOrder
}

// OrdersFor returns an Orders for list, or nil when every entry is nil.
func OrdersFor(list []*KeyOrder) *Orders {
	for _, o := range list {
		if o != nil {
			return &Orders{List: list}
		}
	}
	return nil
}

// Of returns the order of element i, or nil.
func (o *Orders) Of(i int) *KeyOrder {
	if o == nil || i < 0 || i >= len(o.List) {
		return nil
	}
	return o.List[i]
}
