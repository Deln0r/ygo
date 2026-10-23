// Package ygo is a pure-Go port of the Yjs CRDT framework.
//
// ygo is binary-protocol compatible with the npm yjs package (V1 update encoding),
// allowing JavaScript clients to synchronize seamlessly with Go servers and vice versa.
//
// Pure-Go means no CGO, so gomobile bind works for iOS/Android targets.
//
// The public API is stable for the v1.x line: new functionality lands as a
// minor release and breaking changes are deferred to a future major, per
// semantic versioning. The wire formats are validated bidirectionally against
// yjs on every push.
//
// # Strings and values
//
// lib0 reads every string with a fatal UTF-8 decoder, so ygo makes every
// string it is given valid before it becomes part of an item: each byte that
// does not start a valid UTF-8 sequence becomes U+FFFD. That covers text,
// keys, names, values, format attributes, embeds, awareness states and the
// protocol strings the server and client send; UTF-16 indexes into the
// string a caller passed still point at the same places. Lookups by a key
// with such bytes find the fixed key. Values nested deeper than 1000 levels
// are not checked below that depth.
//
// A value passed to the API belongs to the document from then on. Do not
// mutate it after the call, nor a value a read returns, nor an event's
// delta, nor the Branch and Item internals: the document encodes what they
// hold, so a change there can put invalid bytes, or other inconsistencies,
// on the wire. ygo does not copy values defensively: a plain value (strings,
// numbers, bools, nil, []byte, []any, map[string]any) is copied only where a
// string in it needs fixing, and an embed or format value of any other type
// is marshaled once, at the call, and stored as that JSON (a
// json.RawMessage).
//
// See https://github.com/Deln0r/ygo for documentation and examples.
package ygo

// Version is the ygo release this source tree corresponds to. It is set as
// part of the release ceremony, so a checkout of main between releases carries
// the last released value.
const Version = "1.29.0"
