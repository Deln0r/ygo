package lib0

import "testing"

// FuzzReadPrimitives feeds arbitrary bytes to every fixed- and
// variable-width reader. None may panic, hang, or OOM on untrusted
// input, and a successful read must never report consuming more bytes
// than the input holds. ReadVarString and ReadVarUint8Array carry an
// attacker-controlled length prefix, so this is the direct guard on
// that surface — the existing Var* fuzzers only round-trip well-formed
// values and never see hostile bytes.
func FuzzReadPrimitives(f *testing.F) {
	seeds := [][]byte{
		{},
		{0x00},
		{0x7f},
		{0x80, 0x01},
		WriteVarString(nil, "hello"),
		WriteVarUint8Array(nil, []byte{1, 2, 3, 4}),
		// Overlong / huge varuint length prefix: a string or byte slice
		// claiming far more bytes than the buffer holds.
		{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01},
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		bound := func(name string, n int, err error) {
			if err == nil && (n < 0 || n > len(data)) {
				t.Fatalf("%s consumed %d of %d bytes", name, n, len(data))
			}
		}
		_, n, err := ReadVarUint(data)
		bound("ReadVarUint", n, err)
		_, n, err = ReadVarInt(data)
		bound("ReadVarInt", n, err)
		_, n, err = ReadVarString(data)
		bound("ReadVarString", n, err)
		_, n, err = ReadVarUint8Array(data)
		bound("ReadVarUint8Array", n, err)
		_, n, err = ReadUint8(data)
		bound("ReadUint8", n, err)
		_, n, err = ReadUint16(data)
		bound("ReadUint16", n, err)
		_, n, err = ReadUint32(data)
		bound("ReadUint32", n, err)
		_, n, err = ReadFloat32(data)
		bound("ReadFloat32", n, err)
		_, n, err = ReadFloat64(data)
		bound("ReadFloat64", n, err)
	})
}
