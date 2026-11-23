package difffuzz

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
)

// Values travel tagged so that an integer stays an integer in Go:
//
//	["i", 5]  ["f", 1.5]  ["s", "x"]  ["b", true]  ["n"]
//	["a", [v, ...]]  ["o", [["k", v], ...]]
//
// Object keys come in the order ygo writes a Go-built object (array-index
// keys first, ascending, then the rest sorted), which is also the order a
// JavaScript object built in that order iterates.

// DecodeValue builds the Go value of a tagged value: int64, float64,
// string, bool, nil, []any, map[string]any.
func DecodeValue(raw json.RawMessage) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return fromTagged(v)
}

func fromTagged(v any) (any, error) {
	t, ok := v.([]any)
	if !ok || len(t) == 0 {
		return nil, fmt.Errorf("difffuzz: value %v is not tagged", v)
	}
	tag, _ := t[0].(string)
	arg := func() (any, error) {
		if len(t) != 2 {
			return nil, fmt.Errorf("difffuzz: %q wants one argument", tag)
		}
		return t[1], nil
	}
	switch tag {
	case "n":
		return nil, nil
	case "i":
		a, err := arg()
		if err != nil {
			return nil, err
		}
		n, ok := a.(json.Number)
		if !ok {
			return nil, fmt.Errorf("difffuzz: int %v", a)
		}
		return strconv.ParseInt(n.String(), 10, 64)
	case "f":
		a, err := arg()
		if err != nil {
			return nil, err
		}
		n, ok := a.(json.Number)
		if !ok {
			return nil, fmt.Errorf("difffuzz: float %v", a)
		}
		return strconv.ParseFloat(n.String(), 64)
	case "s":
		a, err := arg()
		if err != nil {
			return nil, err
		}
		s, ok := a.(string)
		if !ok {
			return nil, fmt.Errorf("difffuzz: string %v", a)
		}
		return s, nil
	case "b":
		a, err := arg()
		if err != nil {
			return nil, err
		}
		b, ok := a.(bool)
		if !ok {
			return nil, fmt.Errorf("difffuzz: bool %v", a)
		}
		return b, nil
	case "a":
		a, err := arg()
		if err != nil {
			return nil, err
		}
		items, ok := a.([]any)
		if !ok {
			return nil, fmt.Errorf("difffuzz: array %v", a)
		}
		out := make([]any, len(items))
		for i, it := range items {
			if out[i], err = fromTagged(it); err != nil {
				return nil, err
			}
		}
		return out, nil
	case "o":
		a, err := arg()
		if err != nil {
			return nil, err
		}
		pairs, ok := a.([]any)
		if !ok {
			return nil, fmt.Errorf("difffuzz: object %v", a)
		}
		out := make(map[string]any, len(pairs))
		for _, p := range pairs {
			kv, ok := p.([]any)
			if !ok || len(kv) != 2 {
				return nil, fmt.Errorf("difffuzz: object entry %v", p)
			}
			k, ok := kv[0].(string)
			if !ok {
				return nil, fmt.Errorf("difffuzz: object key %v", kv[0])
			}
			if out[k], err = fromTagged(kv[1]); err != nil {
				return nil, err
			}
		}
		return out, nil
	}
	return nil, fmt.Errorf("difffuzz: unknown tag %q", tag)
}

// decodeAttrs builds a Go attribute map, nil when kv is nil (not given).
func decodeAttrs(kv *[]KV) (map[string]any, error) {
	if kv == nil {
		return nil, nil
	}
	out := make(map[string]any, len(*kv))
	for _, e := range *kv {
		v, err := DecodeValue(e.V)
		if err != nil {
			return nil, err
		}
		out[e.K] = v
	}
	return out, nil
}

// Tagged value builders for the generator.
func tagInt(n int64) json.RawMessage     { return mustJSON([]any{"i", n}) }
func tagFloat(f float64) json.RawMessage { return mustJSON([]any{"f", f}) }
func tagStr(s string) json.RawMessage    { return mustJSON([]any{"s", s}) }
func tagBool(b bool) json.RawMessage     { return mustJSON([]any{"b", b}) }
func tagNull() json.RawMessage           { return mustJSON([]any{"n"}) }

func tagArr(items []json.RawMessage) json.RawMessage {
	return mustJSON([]any{"a", items})
}

func tagObj(keys []string, vals []json.RawMessage) json.RawMessage {
	pairs := make([]any, len(keys))
	for i, k := range keys {
		pairs[i] = []any{k, vals[i]}
	}
	return mustJSON([]any{"o", pairs})
}

func mustJSON(v any) json.RawMessage {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		panic(err)
	}
	return json.RawMessage(bytes.TrimRight(buf.Bytes(), "\n"))
}
