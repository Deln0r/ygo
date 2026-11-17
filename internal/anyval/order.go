package anyval

import (
	"sort"
	"strconv"

	"github.com/Deln0r/ygo/internal/block"
)

// JSOrder returns keys, given in insertion order, in the order JavaScript
// enumerates an object's own string keys (Object.keys, JSON.stringify,
// lib0 writeAny): the array-index keys first, in ascending numeric order,
// then the others as given. keys is not modified; without an array-index
// key it is returned as it is.
func JSOrder(keys []string) []string {
	n := 0
	for _, k := range keys {
		if IsArrayIndexKey(k) {
			n++
		}
	}
	if n == 0 {
		return keys
	}
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		if IsArrayIndexKey(k) {
			out = append(out, k)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, _ := strconv.ParseUint(out[i], 10, 32)
		b, _ := strconv.ParseUint(out[j], 10, 32)
		return a < b
	})
	for _, k := range keys {
		if !IsArrayIndexKey(k) {
			out = append(out, k)
		}
	}
	return out
}

// SortedJSOrder returns the keys of m in the order ygo writes an object
// built in Go, which has no insertion order: array-index keys first, in
// ascending numeric order, as JavaScript enumerates them, then the others
// sorted.
func SortedJSOrder[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	index := false
	for k := range m {
		keys = append(keys, k)
		if !index && k != "" && k[0] >= '0' && k[0] <= '9' {
			index = IsArrayIndexKey(k)
		}
	}
	sort.Strings(keys)
	if !index {
		return keys
	}
	return JSOrder(keys)
}

// IsArrayIndexKey reports whether k is an array index in the sense of
// ECMAScript: the canonical decimal form of an integer below 2^32-1.
func IsArrayIndexKey(k string) bool {
	if k == "" || len(k) > 10 || (len(k) > 1 && k[0] == '0') {
		return false
	}
	var n uint64
	for i := 0; i < len(k); i++ {
		c := k[i]
		if c < '0' || c > '9' {
			return false
		}
		n = n*10 + uint64(c-'0')
	}
	return n < 1<<32-1
}

// ObjectKeys returns the keys of m in the order to write them, and the
// order of each key's value: o's keys that m has, in o's order, then m's
// other keys in the fallback order (SortedJSOrder). With no order for m
// it is the fallback order alone. A key o lists but m lacks (an undefined
// entry left out of JSON, a map a caller changed) is skipped.
func ObjectKeys(m map[string]any, o *block.KeyOrder) ([]string, []*block.KeyOrder) {
	if o == nil || o.Keys == nil {
		return SortedJSOrder(m), nil
	}
	// The usual case: the order lists exactly the object's keys.
	if len(o.Keys) == len(m) {
		all := true
		for _, k := range o.Keys {
			if _, ok := m[k]; !ok {
				all = false
				break
			}
		}
		if all {
			return o.Keys, o.Sub
		}
	}
	keys := make([]string, 0, len(m))
	subs := make([]*block.KeyOrder, 0, len(m))
	written := make(map[string]struct{}, len(m))
	for i, k := range o.Keys {
		if _, ok := m[k]; !ok {
			continue
		}
		if _, dup := written[k]; dup {
			continue
		}
		written[k] = struct{}{}
		keys = append(keys, k)
		subs = append(subs, SubOrder(o, i))
	}
	if len(keys) < len(m) {
		for _, k := range SortedJSOrder(m) {
			if _, ok := written[k]; !ok {
				keys = append(keys, k)
				subs = append(subs, nil)
			}
		}
	}
	return keys, subs
}

// SubOrder returns the order of child i of the value o is the order of
// (the value at o.Keys[i] for an object, element i for an array), or nil.
func SubOrder(o *block.KeyOrder, i int) *block.KeyOrder {
	if o == nil || i < 0 || i >= len(o.Sub) {
		return nil
	}
	return o.Sub[i]
}

// IsFallbackOrder reports whether keys, already in JavaScript enumeration
// order (JSOrder), are the order SortedJSOrder writes the same keys in:
// the array-index keys come first and ascending either way, so the rest
// must be ascending.
func IsFallbackOrder(keys []string) bool {
	prev, have := "", false
	for _, k := range keys {
		if IsArrayIndexKey(k) {
			continue
		}
		if have && k <= prev {
			return false
		}
		prev, have = k, true
	}
	return true
}

// NewKeyOrder returns the order of an object whose keys are keys, in
// JavaScript enumeration order, with subs the orders of their values, or
// nil when the fallback order is right for it and everything inside.
func NewKeyOrder(keys []string, subs []*block.KeyOrder) *block.KeyOrder {
	anySub := false
	for _, s := range subs {
		if s != nil {
			anySub = true
			break
		}
	}
	if !anySub && IsFallbackOrder(keys) {
		return nil
	}
	if !anySub {
		subs = nil
	}
	return &block.KeyOrder{Keys: keys, Sub: subs}
}

// ArrayOrder returns the order of an array whose elements have the orders
// subs, or nil when none needs one.
func ArrayOrder(subs []*block.KeyOrder) *block.KeyOrder {
	for _, s := range subs {
		if s != nil {
			return &block.KeyOrder{Sub: subs}
		}
	}
	return nil
}

// OrderOfParsed returns the KeyOrder of a value ParseJSON returned, with
// its keys as the Go value encoding/json decodes from the same text has
// them: a lone surrogate as U+FFFD, and a key that this makes repeat kept
// once, in its first place, with the order of the value read last. It is
// nil where the fallback order is right.
func OrderOfParsed(v any) *block.KeyOrder {
	switch x := v.(type) {
	case *Object:
		keys := make([]string, 0, len(x.Keys))
		subs := make([]*block.KeyOrder, 0, len(x.Keys))
		at := make(map[string]int, len(x.Keys))
		for _, k := range x.Keys {
			sub := OrderOfParsed(x.Vals[k])
			sk := SanitizeJSString(k)
			if i, seen := at[sk]; seen {
				subs[i] = sub
				continue
			}
			at[sk] = len(keys)
			keys = append(keys, sk)
			subs = append(subs, sub)
		}
		return NewKeyOrder(keys, subs)
	case []any:
		subs := make([]*block.KeyOrder, len(x))
		for i, el := range x {
			subs[i] = OrderOfParsed(el)
		}
		return ArrayOrder(subs)
	}
	return nil
}
