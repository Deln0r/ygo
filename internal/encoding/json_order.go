package encoding

import (
	"encoding/json"
	"unicode/utf8"

	"github.com/Deln0r/ygo/internal/block"
)

// jsonOrder returns the key order of the objects in the JSON text s as
// JSON.parse holds them: a key read twice keeps its first place and the
// order of its last value, and array-index keys enumerate first. It is nil
// where the fallback order is right, and shared through t otherwise.
//
// s is text json.Unmarshal accepted, so it is well formed and nests at
// most 10000 levels; the scan does not check it again. Keys are decoded as
// encoding/json decodes them (an invalid byte or a lone surrogate becomes
// U+FFFD), so they are the keys of the value Unmarshal returned for s. A
// text whose objects are all in the fallback order is read without
// allocating.
func jsonOrder(s string, t *orderTable) *block.KeyOrder {
	sc := jsonKeyScanner{s: s, t: t}
	return sc.value()
}

type jsonKeyScanner struct {
	s string
	i int
	t *orderTable
}

func (sc *jsonKeyScanner) space() {
	for sc.i < len(sc.s) {
		switch sc.s[sc.i] {
		case ' ', '\t', '\n', '\r':
			sc.i++
		default:
			return
		}
	}
}

// at reports whether the next byte, past white space, is c, and moves
// past it if so.
func (sc *jsonKeyScanner) at(c byte) bool {
	sc.space()
	if sc.i < len(sc.s) && sc.s[sc.i] == c {
		sc.i++
		return true
	}
	return false
}

// value moves past the value at sc.i and returns its order.
func (sc *jsonKeyScanner) value() *block.KeyOrder {
	sc.space()
	if sc.i >= len(sc.s) {
		return nil
	}
	switch sc.s[sc.i] {
	case '{':
		return sc.object()
	case '[':
		return sc.array()
	case '"':
		sc.str()
	default: // a number, true, false or null
		for sc.i < len(sc.s) {
			switch sc.s[sc.i] {
			case ',', '}', ']', ' ', '\t', '\n', '\r':
				return nil
			}
			sc.i++
		}
	}
	return nil
}

// str moves past the string at sc.i and returns its text without the
// quotes, and whether that text is the string itself: no escape, and
// valid UTF-8, which encoding/json keeps as it is.
func (sc *jsonKeyScanner) str() (string, bool) {
	sc.i++ // the opening quote
	start, plain, high := sc.i, true, false
	for sc.i < len(sc.s) {
		switch c := sc.s[sc.i]; c {
		case '"':
			text := sc.s[start:sc.i]
			sc.i++
			return text, plain && (!high || utf8.ValidString(text))
		case '\\':
			plain = false
			sc.i += 2
		default:
			high = high || c >= utf8.RuneSelf
			sc.i++
		}
	}
	return sc.s[start:], false
}

// key moves past the key at sc.i and returns it as encoding/json decodes
// it.
func (sc *jsonKeyScanner) key() string {
	sc.space()
	from := sc.i
	text, plain := sc.str()
	if plain {
		return text
	}
	var k string
	if err := json.Unmarshal([]byte(sc.s[from:sc.i]), &k); err != nil {
		return text // not reached: Unmarshal read the same string before
	}
	return k
}

func (sc *jsonKeyScanner) object() *block.KeyOrder {
	sc.i++ // '{'
	if sc.at('}') {
		return nil
	}
	// As decodeAnyObjectOrdered reads an object.
	var smallKeys [8]string
	var smallSubs [8]*block.KeyOrder
	keys := smallKeys[:0]
	var subs []*block.KeyOrder
	var at map[string]int
	var kt keyTrack
	for {
		key := sc.key()
		sc.at(':')
		sub := sc.value()
		var p int
		if p, at = keyIndex(keys, at, key); p < 0 {
			p = len(keys)
			keys = append(keys, key)
			if at != nil {
				at[key] = p
			}
			kt.note(key)
		}
		subs = setSub(subs, smallSubs[:0], p, sub)
		if !sc.at(',') {
			sc.at('}')
			break
		}
	}
	return objectOrder(sc.t, keys, subs, at, kt.unsorted, true)
}

func (sc *jsonKeyScanner) array() *block.KeyOrder {
	sc.i++ // '['
	if sc.at(']') {
		return nil
	}
	var small [8]*block.KeyOrder
	var subs []*block.KeyOrder // up to the last element with an order
	for n := 0; ; n++ {
		if sub := sc.value(); sub != nil {
			if subs == nil {
				subs = small[:0]
			}
			for len(subs) < n {
				subs = append(subs, nil)
			}
			subs = append(subs, sub)
		}
		if !sc.at(',') {
			sc.at(']')
			break
		}
	}
	if subs == nil {
		return nil
	}
	return sc.t.array(subs)
}
