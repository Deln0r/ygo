package client

import (
	"testing"

	"github.com/Deln0r/ygo/internal/doc"
	"github.com/Deln0r/ygo/internal/encoding"
	"github.com/Deln0r/ygo/internal/types"
)

// lastSent rises only over what the server's update actually continues.
// Here the application has imported another client's three items from
// elsewhere, and the server then delivers only the first: lastSent may
// claim that one and no more, or the other two would never reach the server.
func TestApply_RaisesLastSentOnlyOverWhatTheServerSent(t *testing.T) {
	x := doc.NewDocWithOptions(doc.Options{ClientID: 7})
	arr := types.NewArray(x.Branch("items"))
	var each [][]byte // one update per transaction: x's clocks 0, 1 and 2
	for _, v := range []string{"a", "b", "c"} {
		var u []byte
		unsub := x.OnUpdate(func(b []byte, _ any) { u = append([]byte(nil), b...) })
		txn := x.WriteTxn()
		arr.Push(txn, v)
		txn.Commit()
		unsub()
		each = append(each, u)
	}
	first := each[0]
	full := encoding.EncodeStateAsUpdate(x)

	c, err := New(Options{URL: "ws://unused", DocName: "room", Doc: doc.NewDocWithOptions(doc.Options{ClientID: 1})})
	if err != nil {
		t.Fatal(err)
	}
	if err := encoding.ApplyUpdate(c.doc, full); err != nil { // imported from elsewhere
		t.Fatal(err)
	}
	if err := c.apply(first); err != nil { // the server delivers x's first item
		t.Fatal(err)
	}
	c.mu.Lock()
	got := c.lastSent[7]
	c.mu.Unlock()
	if got != 1 {
		t.Fatalf("lastSent[7] = %d after the server sent only x's first item, want 1", got)
	}

	// A server update that continues it raises it on; one past a gap does not.
	if err := c.apply(full); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	got = c.lastSent[7]
	c.mu.Unlock()
	if got != 3 {
		t.Fatalf("lastSent[7] = %d after the server sent all of x, want 3", got)
	}
	third, _, err := encoding.DecodeUpdate(each[2])
	if err != nil {
		t.Fatal(err)
	}
	if b := third.Blocks[7]; len(b) != 1 || b[0].Kind != encoding.WireBlockItem || b[0].Item.ID.Clock != 2 {
		t.Fatalf("the third update is not one item at clock 2: %+v", b)
	}
	if cov := serverCoverage(third, map[uint64]uint64{7: 1}, 1); len(cov) != 0 {
		t.Fatalf("coverage of an update that starts past since = %v, want none", cov)
	}
	if cov := serverCoverage(third, map[uint64]uint64{7: 2}, 1); cov[7] != 3 {
		t.Fatalf("coverage of an update that continues since = %v, want 7:3", cov)
	}
	if cov := serverCoverage(third, map[uint64]uint64{}, 7); len(cov) != 0 {
		t.Fatalf("coverage counted the client's own items: %v", cov)
	}
}

// raiseLastSent raises entry by entry, never lowers one, and leaves the map a
// diff may still be reading untouched.
func TestRaiseLastSent_NeverLowers(t *testing.T) {
	c, err := New(Options{URL: "ws://unused", DocName: "room", Doc: doc.NewDocWithOptions(doc.Options{ClientID: 1})})
	if err != nil {
		t.Fatal(err)
	}
	c.raiseLastSent(map[uint64]uint64{7: 5, 8: 1})
	c.mu.Lock()
	before := c.lastSent
	c.mu.Unlock()
	c.raiseLastSent(map[uint64]uint64{7: 3, 9: 2})
	c.mu.Lock()
	after := c.lastSent
	c.mu.Unlock()
	if after[7] != 5 || after[8] != 1 || after[9] != 2 {
		t.Fatalf("lastSent = %v after raising {7:5 8:1} then {7:3 9:2}, want 7:5 8:1 9:2", after)
	}
	if _, ok := before[9]; ok || before[7] != 5 {
		t.Fatalf("the earlier map changed in place: %v", before)
	}
}
