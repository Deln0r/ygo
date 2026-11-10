package block

// Undefined is lib0's undefined (Any tag 127): a map value, or an entry of
// an Any array or object, that yjs stores as undefined. It is not nil,
// which is null, and it survives a decode and an encode as tag 127.
type Undefined struct{}

// MarshalJSON writes null, the closest JSON has. JSON.stringify drops an
// undefined object entry instead; ygo's own JSON renderers do the same.
func (Undefined) MarshalJSON() ([]byte, error) { return []byte("null"), nil }

// BigInt is a JavaScript BigInt (Any tag 122), which lib0 carries as a
// signed 64-bit integer: every BigInt on the wire fits, since lib0's
// writeBigInt64 wraps one outside that range. It is not an int64, which is
// a JavaScript number, and it equals only the same BigInt.
type BigInt int64
