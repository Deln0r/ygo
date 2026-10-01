package ygo_test

import (
	"encoding/hex"
	"testing"

	"github.com/Deln0r/ygo"
)

// Captured from yjs@13.6.33: client 7, map "m" holding two subdocuments,
// {guid: "sub-guid-1"} under "sub" and {guid: "sub-guid-2", autoLoad: true}
// under "auto", in one transaction.
const (
	subdocsV1 = "010207002901016d037375620a7375622d677569642d3176002901016d046175746f0a7375622d677569642d327601086175746f4c6f61647800"
	subdocsV2 = "0000010700000129241d6d7375627375622d677569642d316d6175746f7375622d677569642d3201030a01040a0101000001020076007601086175746f4c6f61647800"
)

// TestV2Subdocs_RoundTrip checks the V2 ContentDoc codec against yjs: the
// V2 state decodes, and re-encodes to the same bytes whether the document
// was loaded from the V2 or the V1 form.
func TestV2Subdocs_RoundTrip(t *testing.T) {
	v1, _ := hex.DecodeString(subdocsV1)
	v2, _ := hex.DecodeString(subdocsV2)
	for _, c := range []struct {
		name  string
		apply func(*ygo.Doc) error
	}{
		{"from V2", func(d *ygo.Doc) error { return ygo.ApplyUpdateV2(d, v2) }},
		{"from V1", func(d *ygo.Doc) error { return ygo.ApplyUpdate(d, v1) }},
	} {
		d := ygo.NewDocWithOptions(ygo.Options{ClientID: 9})
		if err := c.apply(d); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := hex.EncodeToString(ygo.EncodeStateAsUpdateV2(d)); got != subdocsV2 {
			t.Errorf("%s: V2 state\n got  %s\n want %s", c.name, got, subdocsV2)
		}
		if got := hex.EncodeToString(ygo.EncodeStateAsUpdate(d)); got != subdocsV1 {
			t.Errorf("%s: V1 state\n got  %s\n want %s", c.name, got, subdocsV1)
		}
		m := ygo.NewMap(d, "m")
		for key, guid := range map[string]string{"sub": "sub-guid-1", "auto": "sub-guid-2"} {
			sub, ok := m.GetDoc(d, key)
			if !ok || sub.GUID() != guid {
				t.Errorf("%s: %s: ok=%v guid=%v, want %s", c.name, key, ok, sub, guid)
			}
		}
	}
}

// Captured from yjs@13.6.33: client 7, m.sub = new Y.Doc({guid: "g1", gc:
// false, autoLoad: true, meta: {x: 1}}). yjs writes the options in the
// order gc, autoLoad, meta; an encoder that sorts keys writes autoLoad
// first.
const (
	subdocOptsV1 = "010107002901016d03737562026731760302676379086175746f4c6f616478046d657461760101787d0100"
	subdocOptsV2 = "00000107000001290a066d737562673101030201010000010100760302676379086175746f4c6f616478046d657461760101787d0100"
)

func TestSubdocOptions_KeepYjsOrder(t *testing.T) {
	v1, _ := hex.DecodeString(subdocOptsV1)
	v2, _ := hex.DecodeString(subdocOptsV2)
	d := ygo.NewDocWithOptions(ygo.Options{ClientID: 9})
	if err := ygo.ApplyUpdate(d, v1); err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(ygo.EncodeStateAsUpdate(d)); got != subdocOptsV1 {
		t.Errorf("V1\n got  %s\n want %s", got, subdocOptsV1)
	}
	d2 := ygo.NewDocWithOptions(ygo.Options{ClientID: 9})
	if err := ygo.ApplyUpdateV2(d2, v2); err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(ygo.EncodeStateAsUpdateV2(d2)); got != subdocOptsV2 {
		t.Errorf("V2\n got  %s\n want %s", got, subdocOptsV2)
	}
}

// TestOnUpdateV2_Subdoc: creating a subdocument with a V2 update handler
// registered used to panic inside Commit (the V2 encoder had no ContentDoc),
// with the document lock held. The update must now arrive and carry the
// reference to a peer.
func TestOnUpdateV2_Subdoc(t *testing.T) {
	d := ygo.NewDocWithOptions(ygo.Options{ClientID: 1})
	m := ygo.NewMap(d, "m")
	var updates [][]byte
	d.OnUpdateV2(func(u []byte, _ any) { updates = append(updates, u) })
	txn := d.WriteTxn()
	sub := m.SetDoc(txn, "sub")
	txn.Commit()
	if len(updates) != 1 {
		t.Fatalf("got %d V2 updates, want 1", len(updates))
	}
	peer := ygo.NewDocWithOptions(ygo.Options{ClientID: 2})
	if err := ygo.ApplyUpdateV2(peer, updates[0]); err != nil {
		t.Fatal(err)
	}
	got, ok := ygo.NewMap(peer, "m").GetDoc(peer, "sub")
	if !ok || got.GUID() != sub.GUID() {
		t.Fatalf("peer subdoc: ok=%v, want guid %s", ok, sub.GUID())
	}
}
