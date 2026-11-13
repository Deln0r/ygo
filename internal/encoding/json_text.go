package encoding

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"unicode/utf8"

	"github.com/Deln0r/ygo/internal/anyval"
	"github.com/Deln0r/ygo/internal/block"
)

// The V1 format carries embeds and format values as JSON text, the text
// yjs gets from JSON.stringify (UpdateEncoderV1.writeJSON). encoding/json
// writes different text for the same value in several places: it escapes
// &, <, > and U+2028/U+2029, fails the whole value on one NaN or Infinity,
// writes a float32 by its own shortest form where JavaScript sees the
// widened double, and writes a []byte as base64. appendStringify writes
// what JSON.stringify writes for the JavaScript value a peer holds.

// orderedObject is a JSON object with its keys in the order JavaScript
// enumerates them, from JSON text parsed for writing (a json.RawMessage
// value). It is never stored: it lives only while one value is encoded.
type orderedObject struct {
	keys []string
	vals map[string]any
}

// parseOrderedJSON parses raw as JSON.parse does, keeping what a
// JavaScript value keeps and encoding/json drops: each object's keys in
// JavaScript enumeration order (a key seen twice keeps its first position
// and its last value) and lone surrogates in strings, held as their
// three-byte generalized UTF-8 form (appendJSONString writes them back as
// \udxxx escapes, sanitizeJSString turns them into U+FFFD for the wire,
// as lib0's TextEncoder does). Numbers are float64; a literal beyond the
// float64 range is an infinity, as in JavaScript.
func parseOrderedJSON(raw []byte) (any, error) {
	p := &jsonParser{s: raw}
	p.space()
	v, err := p.value(0)
	if err != nil {
		return nil, err
	}
	p.space()
	if p.i != len(p.s) {
		return nil, errors.New("encoding: trailing data after JSON value")
	}
	return v, nil
}

type jsonParser struct {
	s []byte
	i int
}

var errJSONSyntax = errors.New("encoding: invalid JSON text")

func (p *jsonParser) space() {
	for p.i < len(p.s) {
		switch p.s[p.i] {
		case ' ', '\t', '\n', '\r':
			p.i++
		default:
			return
		}
	}
}

func (p *jsonParser) literal(lit string) bool {
	if bytes.HasPrefix(p.s[p.i:], []byte(lit)) {
		p.i += len(lit)
		return true
	}
	return false
}

func (p *jsonParser) value(depth int) (any, error) {
	if depth > anyval.MaxTreeDepth || p.i >= len(p.s) {
		return nil, errJSONSyntax
	}
	switch c := p.s[p.i]; {
	case c == '{':
		p.i++
		obj := &orderedObject{vals: map[string]any{}}
		var keys []string
		p.space()
		if p.i < len(p.s) && p.s[p.i] == '}' {
			p.i++
			return obj, nil
		}
		for {
			p.space()
			if p.i >= len(p.s) || p.s[p.i] != '"' {
				return nil, errJSONSyntax
			}
			k, err := p.str()
			if err != nil {
				return nil, err
			}
			p.space()
			if p.i >= len(p.s) || p.s[p.i] != ':' {
				return nil, errJSONSyntax
			}
			p.i++
			p.space()
			v, err := p.value(depth + 1)
			if err != nil {
				return nil, err
			}
			if _, seen := obj.vals[k]; !seen {
				keys = append(keys, k)
			}
			obj.vals[k] = v
			p.space()
			if p.i < len(p.s) && p.s[p.i] == ',' {
				p.i++
				continue
			}
			if p.i < len(p.s) && p.s[p.i] == '}' {
				p.i++
				obj.keys = anyval.JSOrder(keys)
				return obj, nil
			}
			return nil, errJSONSyntax
		}
	case c == '[':
		p.i++
		arr := []any{}
		p.space()
		if p.i < len(p.s) && p.s[p.i] == ']' {
			p.i++
			return arr, nil
		}
		for {
			p.space()
			v, err := p.value(depth + 1)
			if err != nil {
				return nil, err
			}
			arr = append(arr, v)
			p.space()
			if p.i < len(p.s) && p.s[p.i] == ',' {
				p.i++
				continue
			}
			if p.i < len(p.s) && p.s[p.i] == ']' {
				p.i++
				return arr, nil
			}
			return nil, errJSONSyntax
		}
	case c == '"':
		return p.str()
	case c == 't' && p.literal("true"):
		return true, nil
	case c == 'f' && p.literal("false"):
		return false, nil
	case c == 'n' && p.literal("null"):
		return nil, nil
	case c == '-' || (c >= '0' && c <= '9'):
		return p.number()
	}
	return nil, errJSONSyntax
}

func (p *jsonParser) number() (any, error) {
	start := p.i
	if p.s[p.i] == '-' {
		p.i++
	}
	digits := func() int {
		n := 0
		for p.i < len(p.s) && p.s[p.i] >= '0' && p.s[p.i] <= '9' {
			p.i++
			n++
		}
		return n
	}
	if p.i < len(p.s) && p.s[p.i] == '0' {
		p.i++
	} else if digits() == 0 {
		return nil, errJSONSyntax
	}
	if p.i < len(p.s) && p.s[p.i] == '.' {
		p.i++
		if digits() == 0 {
			return nil, errJSONSyntax
		}
	}
	if p.i < len(p.s) && (p.s[p.i] == 'e' || p.s[p.i] == 'E') {
		p.i++
		if p.i < len(p.s) && (p.s[p.i] == '+' || p.s[p.i] == '-') {
			p.i++
		}
		if digits() == 0 {
			return nil, errJSONSyntax
		}
	}
	f, err := strconv.ParseFloat(string(p.s[start:p.i]), 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return nil, errJSONSyntax
	}
	return f, nil // ErrRange: ±Inf or a rounded tiny value, as JavaScript reads it
}

// str reads a JSON string. A \u escape of a lone surrogate is kept as its
// generalized UTF-8 bytes (ED A0..BF xx), as a JavaScript string keeps it.
func (p *jsonParser) str() (string, error) {
	p.i++ // opening quote
	var b []byte
	for p.i < len(p.s) {
		c := p.s[p.i]
		switch {
		case c == '"':
			p.i++
			return string(b), nil
		case c < 0x20:
			return "", errJSONSyntax
		case c != '\\':
			b = append(b, c)
			p.i++
			continue
		}
		p.i++
		if p.i >= len(p.s) {
			return "", errJSONSyntax
		}
		e := p.s[p.i]
		p.i++
		switch e {
		case '"', '\\', '/':
			b = append(b, e)
		case 'b':
			b = append(b, '\b')
		case 'f':
			b = append(b, '\f')
		case 'n':
			b = append(b, '\n')
		case 'r':
			b = append(b, '\r')
		case 't':
			b = append(b, '\t')
		case 'u':
			r, ok := p.hex4()
			if !ok {
				return "", errJSONSyntax
			}
			if r >= 0xD800 && r <= 0xDBFF && p.i+6 <= len(p.s) && p.s[p.i] == '\\' && p.s[p.i+1] == 'u' {
				save := p.i
				p.i += 2
				if lo, ok := p.hex4(); ok && lo >= 0xDC00 && lo <= 0xDFFF {
					b = utf8.AppendRune(b, 0x10000+(r-0xD800)<<10+(lo-0xDC00))
					continue
				}
				p.i = save
			}
			if r >= 0xD800 && r <= 0xDFFF {
				b = append(b, 0xE0|byte(r>>12), 0x80|byte(r>>6)&0x3F, 0x80|byte(r)&0x3F)
				continue
			}
			b = utf8.AppendRune(b, r)
		default:
			return "", errJSONSyntax
		}
	}
	return "", errJSONSyntax
}

func (p *jsonParser) hex4() (rune, bool) {
	if p.i+4 > len(p.s) {
		return 0, false
	}
	var r rune
	for _, c := range p.s[p.i : p.i+4] {
		r <<= 4
		switch {
		case c >= '0' && c <= '9':
			r |= rune(c - '0')
		case c >= 'a' && c <= 'f':
			r |= rune(c - 'a' + 10)
		case c >= 'A' && c <= 'F':
			r |= rune(c - 'A' + 10)
		default:
			return 0, false
		}
	}
	p.i += 4
	return r, true
}

// surrogateAt reports whether s[i:] starts with a lone surrogate in
// generalized UTF-8 form, and its code unit.
func surrogateAt(s string, i int) (rune, bool) {
	if i+2 < len(s) && s[i] == 0xED && s[i+1] >= 0xA0 && s[i+1] <= 0xBF && s[i+2] >= 0x80 && s[i+2] <= 0xBF {
		return rune(s[i]&0x0F)<<12 | rune(s[i+1]&0x3F)<<6 | rune(s[i+2]&0x3F), true
	}
	return 0, false
}

// sanitizeJSString returns s with every lone surrogate replaced by U+FFFD,
// one for each, as lib0's TextEncoder writes a JavaScript string with one.
func sanitizeJSString(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	var b []byte
	for i := 0; i < len(s); {
		if _, ok := surrogateAt(s, i); ok {
			b = utf8.AppendRune(b, utf8.RuneError)
			i += 3
			continue
		}
		r, n := utf8.DecodeRuneInString(s[i:])
		b = utf8.AppendRune(b, r)
		i += n
	}
	return string(b)
}

// appendStringify appends JSON.stringify of the JavaScript value v stands
// for. v has been through anyval.ForJSON, so undefined object entries are
// gone, other undefineds are nil, -0 is 0 and a BigInt is a json.Number.
// Objects built in Go are written with array-index keys first in ascending
// order, then the others sorted (anyval.SortedJSOrder); objects parsed from
// JSON text keep their order.
func appendStringify(b []byte, v any) []byte {
	switch x := v.(type) {
	case nil, block.Undefined:
		return append(b, "null"...)
	case bool:
		if x {
			return append(b, "true"...)
		}
		return append(b, "false"...)
	case string:
		return appendJSONString(b, x)
	case json.Number:
		return append(b, x...)
	case float64:
		return appendJSNumber(b, x)
	case float32:
		return appendJSNumber(b, float64(x))
	case int:
		return appendJSNumber(b, float64(x))
	case int8:
		return appendJSNumber(b, float64(x))
	case int16:
		return appendJSNumber(b, float64(x))
	case int32:
		return appendJSNumber(b, float64(x))
	case int64:
		return appendJSNumber(b, float64(x))
	case uint:
		return appendJSNumber(b, float64(x))
	case uint8:
		return appendJSNumber(b, float64(x))
	case uint16:
		return appendJSNumber(b, float64(x))
	case uint32:
		return appendJSNumber(b, float64(x))
	case uint64:
		return appendJSNumber(b, float64(x))
	case []byte:
		// A Uint8Array: JSON.stringify writes its indexed elements as an
		// object, {"0":1,"1":2}.
		if x == nil {
			return append(b, "null"...)
		}
		b = append(b, '{')
		for i, c := range x {
			if i > 0 {
				b = append(b, ',')
			}
			b = append(b, '"')
			b = strconv.AppendInt(b, int64(i), 10)
			b = append(b, '"', ':')
			b = strconv.AppendUint(b, uint64(c), 10)
		}
		return append(b, '}')
	case []any:
		if x == nil {
			return append(b, "null"...)
		}
		b = append(b, '[')
		for i, el := range x {
			if i > 0 {
				b = append(b, ',')
			}
			b = appendStringify(b, el)
		}
		return append(b, ']')
	case map[string]any:
		if x == nil {
			return append(b, "null"...)
		}
		return appendObject(b, anyval.SortedJSOrder(x), x)
	case *orderedObject:
		return appendObject(b, x.keys, x.vals)
	case json.RawMessage:
		parsed, err := parseOrderedJSON(x)
		if err != nil {
			return append(b, "null"...)
		}
		return appendStringify(b, parsed)
	default:
		raw, err := json.Marshal(v)
		if err != nil {
			return append(b, "null"...)
		}
		return appendStringify(b, json.RawMessage(raw))
	}
}

func appendObject(b []byte, keys []string, vals map[string]any) []byte {
	b = append(b, '{')
	first := true
	for _, k := range keys {
		el, ok := vals[k]
		if !ok {
			continue
		}
		if _, undefined := el.(block.Undefined); undefined {
			continue // JSON.stringify leaves an undefined property out
		}
		if !first {
			b = append(b, ',')
		}
		first = false
		b = appendJSONString(b, k)
		b = append(b, ':')
		b = appendStringify(b, el)
	}
	return append(b, '}')
}

// appendJSNumber appends a number as JSON.stringify writes it: NaN and the
// infinities as null, zero (either sign) as 0, otherwise as JavaScript's
// Number.prototype.toString does, plain digits for magnitudes from 1e-6 up
// to below 1e21 and exponent notation outside (1e+21, 1e-7).
func appendJSNumber(b []byte, f float64) []byte {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return append(b, "null"...)
	}
	if f == 0 {
		return append(b, '0')
	}
	abs := math.Abs(f)
	if abs >= 1e-6 && abs < 1e21 {
		return strconv.AppendFloat(b, f, 'f', -1, 64)
	}
	start := len(b)
	b = strconv.AppendFloat(b, f, 'e', -1, 64)
	// Go writes e-07 and e+07 where JavaScript writes e-7 and e+7.
	if n := len(b); n-start >= 4 && b[n-4] == 'e' && b[n-2] == '0' {
		b[n-2] = b[n-1]
		b = b[:n-1]
	}
	return b
}

const hexDigits = "0123456789abcdef"

// appendJSONString appends s quoted as JSON.stringify quotes a string:
// only the quote, the backslash and the control characters below U+0020
// are escaped (\b \t \n \f \r, the rest as \u00xx); everything else,
// & < > U+2028 U+2029 included, is written as it is.
func appendJSONString(b []byte, s string) []byte {
	b = append(b, '"')
	start := 0
	for i := 0; i < len(s); {
		c := s[i]
		if c >= utf8.RuneSelf {
			// A lone surrogate (from JSON text, parseOrderedJSON) is
			// escaped, as well-formed JSON.stringify escapes it.
			if r, ok := surrogateAt(s, i); ok {
				b = append(b, s[start:i]...)
				b = append(b, '\\', 'u', hexDigits[r>>12], hexDigits[r>>8&0xf], hexDigits[r>>4&0xf], hexDigits[r&0xf])
				i += 3
				start = i
				continue
			}
			i++
			continue
		}
		var esc string
		switch c {
		case '"':
			esc = `\"`
		case '\\':
			esc = `\\`
		case '\b':
			esc = `\b`
		case '\f':
			esc = `\f`
		case '\n':
			esc = `\n`
		case '\r':
			esc = `\r`
		case '\t':
			esc = `\t`
		default:
			if c >= 0x20 {
				i++
				continue
			}
		}
		b = append(b, s[start:i]...)
		if esc != "" {
			b = append(b, esc...)
		} else {
			b = append(b, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0xf])
		}
		i++
		start = i
	}
	b = append(b, s[start:]...)
	return append(b, '"')
}
