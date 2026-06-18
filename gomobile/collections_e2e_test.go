package gomobile_test

import (
	"strings"
	"testing"

	"github.com/Deln0r/ygo/gomobile"
)

// TestMobile_Collections_RoundTrip drives the full type set a real app
// uses — typed Array values, typed and nested Map entries, and an XML
// tree — through a real server, asserting each lands on the peer.
func TestMobile_Collections_RoundTrip(t *testing.T) {
	url := startServer(t)

	da := gomobile.NewDocWithClientID(91)
	ca := gomobile.NewClient(url, "coll", da)
	if err := ca.Connect(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ca.Close() })

	db := gomobile.NewDocWithClientID(92)
	cb := gomobile.NewClient(url, "coll", db)
	if err := cb.Connect(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cb.Close() })

	waitFor(t, "a synced", ca.Synced)
	waitFor(t, "b synced", cb.Synced)

	// Array of typed scalar values.
	aa := da.Array("list")
	for _, v := range []string{`42`, `true`, `"hi"`} {
		if err := aa.PushJSON([]byte(v)); err != nil {
			t.Fatal(err)
		}
	}
	ab := db.Array("list")
	waitFor(t, "b sees array", func() bool { return ab.Length() == 3 })
	if got := string(ab.GetJSON(0)); got != "42" {
		t.Errorf("array[0] = %s, want 42", got)
	}
	if got := string(ab.ToJSON()); got != `[42,true,"hi"]` {
		t.Errorf("array = %s, want [42,true,\"hi\"]", got)
	}

	// Map: a typed scalar plus a nested Map.
	ma := da.Map("meta")
	if err := ma.SetJSON("count", []byte(`7`)); err != nil {
		t.Fatal(err)
	}
	opts := ma.SetMap("opts")
	if err := opts.SetJSON("dark", []byte(`true`)); err != nil {
		t.Fatal(err)
	}
	mb := db.Map("meta")
	waitFor(t, "b sees count", func() bool { return string(mb.GetJSON("count")) == "7" })
	waitFor(t, "b sees nested map", func() bool {
		nb := mb.GetMap("opts")
		return nb != nil && string(nb.GetJSON("dark")) == "true"
	})

	// XML element tree with an attribute and a text node.
	frag := da.XmlFragment("doc")
	p := frag.InsertElement(0, "paragraph")
	p.SetAttribute("align", "center")
	xt := p.InsertText(0)
	if err := xt.Text().InsertAt(0, "hello"); err != nil {
		t.Fatal(err)
	}
	fragB := db.XmlFragment("doc")
	waitFor(t, "b sees xml text", func() bool { return strings.Contains(fragB.ToString(), "hello") })
	waitFor(t, "b sees xml attr", func() bool {
		eb := fragB.GetElement(0)
		return eb != nil && eb.GetAttribute("align") == "center"
	})
}
