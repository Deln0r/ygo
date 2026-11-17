package ygo_test

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	"github.com/Deln0r/ygo"
)

// TestRootNamedEmptyString: a root type named "" (yjs doc.getMap("")) is
// written back in V1 and V2, a relative position anchors on it, and a
// late joiner gets it. ygo applied such an update and then panicked
// writing the document, which a peer could use to stop a server sending
// the document to anyone else.
func TestRootNamedEmptyString(t *testing.T) {
	raw, err := os.ReadFile("testdata/wire-edge-fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Scenarios []struct {
			Description string `json:"description"`
			UpdateHex   string `json:"update_hex"`
		} `json:"scenarios"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	var u []byte
	for _, s := range f.Scenarios {
		if s.Description == "root type named the empty string" {
			u, _ = hex.DecodeString(s.UpdateHex)
		}
	}
	if u == nil {
		t.Fatal("no fixture")
	}
	d := ygo.NewDoc()
	m := ygo.NewMap(d, "")
	if err := ygo.ApplyUpdate(d, u); err != nil {
		t.Fatal(err)
	}
	if got := m.Get("0"); got != "v" {
		t.Fatalf("m[0] = %v", got)
	}
	if got := ygo.EncodeStateAsUpdate(d); !bytes.Equal(got, u) {
		t.Errorf("V1:\n got %x\nwant %x", got, u)
	}
	r := ygo.NewDoc()
	if err := ygo.ApplyUpdateV2(r, ygo.EncodeStateAsUpdateV2(d)); err != nil {
		t.Fatal(err)
	}
	if got := ygo.EncodeStateAsUpdate(r); !bytes.Equal(got, u) {
		t.Errorf("V2 round trip:\n got %x\nwant %x", got, u)
	}

	// A text root named "" written from Go, and a position at its end.
	g := ygo.NewDocWithOptions(ygo.Options{ClientID: 1})
	x := ygo.NewText(g, "")
	txn := g.WriteTxn()
	if err := x.Insert(txn, 0, "ab"); err != nil {
		t.Fatal(err)
	}
	txn.Commit()
	rp, err := ygo.CreateRelativePositionFromTypeIndex(x, 2, 0)
	if err != nil {
		t.Fatalf("relative position at the end of a root named \"\": %v", err)
	}
	enc, err := ygo.EncodeRelativePosition(rp)
	if err != nil {
		t.Fatal(err)
	}
	// yjs: tag 1 (tname), varstring "", assoc 0.
	if want := []byte{1, 0, 0}; !bytes.Equal(enc, want) {
		t.Errorf("relative position %x, want %x", enc, want)
	}
}
