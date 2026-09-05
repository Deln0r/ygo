package encoding

import (
	"testing"

	"github.com/Deln0r/ygo/internal/block"
)

// TestDecodeSkipStruct verifies the V1 decoder recognises a Skip struct
// (ref number 10), which represents a hole in a client's run.
//
// Not a forward-compatibility exercise, though it was written as one:
// measured against yjs 13.6.32, mergeUpdates emits Skip runs today, for the
// ordinary case of merging a set that spans a gap. ygo emits them too now
// (EncodeStateAsUpdateWithPending), so both halves of the format are in use
// rather than merely tolerated. A decoder that only knew refs 0-9 would error
// on the leading info byte.
func TestDecodeSkipStruct(t *testing.T) {
	// numClients=1, blockCount=1, client=5, clock=0,
	// Skip block (info=10, len=3), empty delete set.
	buf := []byte{0x01, 0x01, 0x05, 0x00, 0x0a, 0x03, 0x00}
	u, tail, err := DecodeUpdate(buf)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(tail) != 0 {
		t.Errorf("trailing bytes: %x", tail)
	}
	blocks := u.Blocks[5]
	if len(blocks) != 1 {
		t.Fatalf("got %d blocks, want 1", len(blocks))
	}
	b := blocks[0]
	if b.Kind != WireBlockSkip {
		t.Errorf("kind = %v, want WireBlockSkip", b.Kind)
	}
	if (b.ID != block.ID{Client: 5, Clock: 0}) || b.Len != 3 {
		t.Errorf("got ID %v len %d, want {5,0} len 3", b.ID, b.Len)
	}
}
