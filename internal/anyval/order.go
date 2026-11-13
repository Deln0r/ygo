package anyval

import (
	"sort"
	"strconv"
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
