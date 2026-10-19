package ygo_test

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/Deln0r/ygo"
	"github.com/Deln0r/ygo/internal/awareness"
	"github.com/Deln0r/ygo/internal/encoding"
	"github.com/Deln0r/ygo/internal/lib0"
	syncpkg "github.com/Deln0r/ygo/internal/sync"
)

// strictRoundTrip encodes d in V1 and V2 and applies both, strictly, to new
// documents: the bytes ygo writes must be bytes lib0 reads. The reverse
// fixtures (gen-go-fixtures, validate-go-fixtures.mjs) check the same bytes
// against yjs.
func strictRoundTrip(t *testing.T, d *ygo.Doc) (*ygo.Doc, *ygo.Doc) {
	t.Helper()
	v1, v2 := ygo.NewDoc(), ygo.NewDoc()
	if err := ygo.ApplyUpdate(v1, ygo.EncodeStateAsUpdate(d)); err != nil {
		t.Fatalf("V1: %v", err)
	}
	if err := ygo.ApplyUpdateV2(v2, ygo.EncodeStateAsUpdateV2(d)); err != nil {
		t.Fatalf("V2: %v", err)
	}
	return v1, v2
}

// Two fragments of one character inserted in separate calls by one client
// are fixed one by one, before the commit merges the two items: two U+FFFD,
// length 2 everywhere, and a replica that holds only the first character
// takes the second through a diff from clock 1, in V1 and in V2.
func TestAPI_InvalidUTF8_FragmentsDoNotRecombine(t *testing.T) {
	d := ygo.NewDocWithOptions(ygo.Options{ClientID: 1})
	txt := ygo.NewText(d, "t")
	w := d.WriteTxn()
	if err := txt.Insert(w, 0, "\xc2"); err != nil {
		t.Fatal(err)
	}
	w.Commit()
	w = d.WriteTxn()
	if err := txt.Insert(w, 1, "\xa2"); err != nil {
		t.Fatal(err)
	}
	w.Commit()
	if got := txt.String(); got != "\uFFFD\uFFFD" || txt.Length() != 2 {
		t.Fatalf("text %q length %d, want two U+FFFD, length 2", got, txt.Length())
	}
	v1, v2 := strictRoundTrip(t, d)
	for _, r := range []*ygo.Doc{v1, v2} {
		if got := ygo.NewText(r, "t").String(); got != "\uFFFD\uFFFD" {
			t.Errorf("replica text %q", got)
		}
	}

	first := ygo.NewDocWithOptions(ygo.Options{ClientID: 1})
	ft := ygo.NewText(first, "t")
	w = first.WriteTxn()
	if err := ft.Insert(w, 0, "\xc2"); err != nil {
		t.Fatal(err)
	}
	w.Commit()
	sv := encoding.EncodeStateVector(map[uint64]uint64{1: 1}, nil)
	diff, err := ygo.EncodeDiff(d, sv)
	if err != nil {
		t.Fatal(err)
	}
	diffV2, err := ygo.EncodeDiffV2(d, sv)
	if err != nil {
		t.Fatal(err)
	}
	for name, apply := range map[string]func(*ygo.Doc) error{
		"V1": func(r *ygo.Doc) error { return ygo.ApplyUpdate(r, diff) },
		"V2": func(r *ygo.Doc) error { return ygo.ApplyUpdateV2(r, diffV2) },
	} {
		r := ygo.NewDoc()
		if err := ygo.ApplyUpdate(r, ygo.EncodeStateAsUpdate(first)); err != nil {
			t.Fatal(err)
		}
		if err := apply(r); err != nil {
			t.Fatalf("%s diff from clock 1: %v", name, err)
		}
		if got := ygo.NewText(r, "t").String(); got != "\uFFFD\uFFFD" {
			t.Errorf("%s replica after the diff: %q", name, got)
		}
	}
}

// Keys are fixed at Set and at lookup, so a lookup by the raw key finds what
// Set stored, and the U+FFFD form is the same key.
func TestAPI_InvalidUTF8_KeyLookup(t *testing.T) {
	d := ygo.NewDoc()
	m := ygo.NewMap(d, "m")
	w := d.WriteTxn()
	m.Set(w, "\xff", int64(1))
	w.Commit()
	if got := m.Get("\xff"); got != int64(1) {
		t.Fatalf("Get raw key = %v, want 1", got)
	}
	w = d.WriteTxn()
	m.Set(w, "\uFFFD", int64(2))
	w.Commit()
	if got := m.Get("\xff"); got != int64(2) {
		t.Fatalf("Get raw key after Set of U+FFFD = %v, want 2 (same key)", got)
	}
	if !m.Has("\xff") {
		t.Fatal("Has raw key = false")
	}
	w = d.WriteTxn()
	m.Delete(w, "\xff")
	w.Commit()
	if m.Has("\uFFFD") {
		t.Fatal("Delete by the raw key left the entry")
	}
}

// A value is copied only where a string needs fixing; the caller's value is
// never changed, and a valid value is stored as it is.
func TestAPI_InvalidUTF8_CallerValueUntouched(t *testing.T) {
	d := ygo.NewDoc()
	m := ygo.NewMap(d, "m")
	bad := map[string]any{"k\xff": []any{"v\xfe"}}
	good := map[string]any{"k": []any{"v"}}
	w := d.WriteTxn()
	m.Set(w, "bad", bad)
	m.Set(w, "good", good)
	w.Commit()
	if _, ok := bad["k\xff"]; !ok || bad["k\xff"].([]any)[0] != "v\xfe" {
		t.Fatalf("the caller's map changed: %#v", bad)
	}
	stored := m.Get("bad").(map[string]any)
	if v, ok := stored["k\uFFFD"]; !ok || v.([]any)[0] != "v\uFFFD" {
		t.Fatalf("stored %#v, want the fixed key and value", stored)
	}
	if reflect.ValueOf(m.Get("good")).Pointer() != reflect.ValueOf(good).Pointer() {
		t.Error("a valid value was copied")
	}
	strictRoundTrip(t, d)
}

// changing marshals to a different value on every call. A non-plain embed
// value is marshaled once, at the call, and stored as that JSON, so every
// encoding of the item carries the same value.
type changing struct{ n *int }

func (c changing) MarshalJSON() ([]byte, error) {
	*c.n++
	return json.Marshal(map[string]any{"call": *c.n, "s": "x\xff"})
}

func TestAPI_InvalidUTF8_MarshalerConvertedOnce(t *testing.T) {
	d := ygo.NewDoc()
	txt := ygo.NewText(d, "t")
	n := 0
	w := d.WriteTxn()
	if err := txt.InsertEmbed(w, 0, changing{n: &n}); err != nil {
		t.Fatal(err)
	}
	w.Commit()
	want := map[string]any{"call": float64(1), "s": "x\uFFFD"}
	local := txt.ToDelta()
	raw, ok := local[0].Embed.(json.RawMessage)
	if len(local) != 1 || !ok {
		t.Fatalf("local delta %#v, want one embed stored as its JSON", local)
	}
	var localValue any
	if err := json.Unmarshal(raw, &localValue); err != nil || !reflect.DeepEqual(localValue, want) {
		t.Fatalf("local embed %s, want %#v", raw, want)
	}
	for i := 0; i < 2; i++ { // encode twice: the value must not change
		v1, v2 := strictRoundTrip(t, d)
		for name, r := range map[string]*ygo.Doc{"V1": v1, "V2": v2} {
			got := ygo.NewText(r, "t").ToDelta()
			if len(got) != 1 || !reflect.DeepEqual(got[0].Embed, want) {
				t.Errorf("round %d %s delta %#v, want %#v", i, name, got, want)
			}
		}
	}
	if n != 1 {
		t.Errorf("MarshalJSON called %d times, want once (at the call)", n)
	}
}

// An embed or format value of a non-plain Go type is stored as its JSON, so
// replaying ToDelta keeps each embed one position, a JSON null included; two
// Format calls with the same such value are one change, not two; and escaped
// surrogates in the JSON text stay as they are in V1.
func TestAPI_JSONValuesStoredAsJSON(t *testing.T) {
	d := ygo.NewDoc()
	txt := ygo.NewText(d, "t")
	w := d.WriteTxn()
	if err := txt.InsertEmbed(w, 0, json.RawMessage(`"abc"`)); err != nil {
		t.Fatal(err)
	}
	if err := txt.InsertEmbed(w, 1, json.RawMessage(`null`)); err != nil {
		t.Fatal(err)
	}
	w.Commit()
	other := ygo.NewDoc()
	ot := ygo.NewText(other, "t")
	w = other.WriteTxn()
	if err := ot.ApplyDelta(w, txt.ToDelta()); err != nil {
		t.Fatal(err)
	}
	w.Commit()
	if ot.Length() != 2 || len(ot.ToDelta()) != 2 {
		t.Fatalf("replayed delta: length %d, %#v; want two embeds", ot.Length(), ot.ToDelta())
	}

	v := map[string]map[string]int{"x": {"y": 1}}
	fd := ygo.NewDocWithOptions(ygo.Options{ClientID: 5})
	ft := ygo.NewText(fd, "t")
	w = fd.WriteTxn()
	if err := ft.Insert(w, 0, "abc"); err != nil {
		t.Fatal(err)
	}
	if err := ft.Format(w, 0, 3, ygo.Attrs{"meta": v}); err != nil {
		t.Fatal(err)
	}
	w.Commit()
	before := ygo.EncodeStateVector(fd)
	w = fd.WriteTxn()
	if err := ft.Format(w, 0, 3, ygo.Attrs{"meta": v}); err != nil {
		t.Fatal(err)
	}
	w.Commit()
	if after := ygo.EncodeStateVector(fd); !bytes.Equal(before, after) {
		t.Errorf("the second Format with the same value wrote items")
	}

	sd := ygo.NewDoc()
	st := ygo.NewText(sd, "t")
	w = sd.WriteTxn()
	if err := st.InsertEmbed(w, 0, json.RawMessage(`{"\ud800":1,"\ud801":2}`)); err != nil {
		t.Fatal(err)
	}
	w.Commit()
	u := ygo.EncodeStateAsUpdate(sd)
	if !bytes.Contains(u, []byte(`\ud800`)) || !bytes.Contains(u, []byte(`\ud801`)) {
		t.Errorf("V1 update lost an escaped key: %q", u)
	}
}

// Root names, XML element and attribute names and format attribute keys
// and values are fixed at the call; lookups by the raw name find them, and
// replicas hold the fixed names and attributes.
func TestAPI_InvalidUTF8_NamesAndAttributes(t *testing.T) {
	d := ygo.NewDoc()
	txt := ygo.NewText(d, "r\xff")
	frag := ygo.NewXmlFragment(d, "f\xfe")
	w := d.WriteTxn()
	if err := txt.InsertWithAttributes(w, 0, "ab", ygo.Attrs{"k\xff": "v\xfe"}); err != nil {
		t.Fatal(err)
	}
	el := frag.InsertXmlElement(w, 0, "p\xff")
	el.SetAttribute(w, "a\xfe", "v\xfd")
	w.Commit()
	if v, ok := el.GetAttribute("a\xfe"); !ok || v != "v\uFFFD" {
		t.Fatalf("GetAttribute by the raw name = %q, %v", v, ok)
	}
	v1, v2 := strictRoundTrip(t, d)
	for name, r := range map[string]*ygo.Doc{"V1": v1, "V2": v2} {
		got := ygo.NewText(r, "r\uFFFD").ToDelta()
		want := []ygo.DeltaOp{{Insert: "ab", Attributes: ygo.Attrs{"k\uFFFD": "v\uFFFD"}}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: delta %#v, want %#v", name, got, want)
		}
		el, ok := ygo.NewXmlFragment(r, "f\uFFFD").Get(0).(*ygo.XmlElement)
		if !ok || el.NodeName() != "p\uFFFD" {
			t.Errorf("%s: element %#v", name, el)
			continue
		}
		if v, ok := el.GetAttribute("a\uFFFD"); !ok || v != "v\uFFFD" {
			t.Errorf("%s: attribute %q, %v", name, v, ok)
		}
	}
}

// Protocol strings, a hand-built relative position and an awareness state
// with invalid bytes encode bytes lib0 reads.
func TestAPI_InvalidUTF8_ProtocolStrings(t *testing.T) {
	for name, frame := range map[string][]byte{
		"auth token":        syncpkg.EncodeAuthToken("t\xff"),
		"permission denied": syncpkg.EncodeAuthPermissionDenied("r\xff"),
		"stateless":         syncpkg.EncodeStateless("s\xff"),
		"broadcast":         syncpkg.EncodeBroadcastStateless("b\xff"),
		"close":             syncpkg.EncodeClose("c\xff"),
	} {
		f, _, err := syncpkg.DecodeEnvelope(frame)
		if err != nil {
			t.Errorf("%s: envelope: %v", name, err)
			continue
		}
		if _, _, err := lib0.ReadVarString(trimToString(t, name, frame, f)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}

	raw := "r\xff"
	enc, err := ygo.EncodeRelativePosition(ygo.RelativePosition{TName: &raw})
	if err != nil {
		t.Fatal(err)
	}
	rp, err := ygo.DecodeRelativePosition(enc)
	if err != nil {
		t.Fatalf("relative position: %v", err)
	}
	if rp.TName == nil || *rp.TName != "r\uFFFD" {
		t.Fatalf("relative position name %v", rp.TName)
	}

	a := ygo.NewAwareness(7)
	a.SetLocalState([]byte(`{"name":"n` + "\xff" + `"}`))
	b := ygo.NewAwareness(8)
	if _, err := b.Apply(a.Encode([]uint64{7}), nil); err != nil {
		t.Fatalf("awareness: %v", err)
	}
}

// An awareness state that differs from the previous one only in bytes that
// get fixed is the same state: OnChange does not fire again.
func TestAPI_AwarenessSameFixedStateNoChange(t *testing.T) {
	a := ygo.NewAwareness(7)
	changes := 0
	a.OnChange(func(_ awareness.Summary, _ any) { changes++ })
	for i := 0; i < 2; i++ {
		a.SetLocalState([]byte(`{"x":"` + "\xff" + `"}`))
	}
	if changes != 1 {
		t.Errorf("OnChange fired %d times, want 1", changes)
	}
}

// trimToString returns the bytes of frame from where its string field starts:
// after the message type, and for auth after the auth sub-type.
func trimToString(t *testing.T, name string, frame []byte, f *syncpkg.Frame) []byte {
	t.Helper()
	_, n, err := lib0.ReadVarUint(frame)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	rest := frame[n:]
	if f.Type == syncpkg.MessageAuth {
		_, n, err = lib0.ReadVarUint(rest)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		rest = rest[n:]
	}
	return rest
}

// A value stored as its JSON is not equal to a different value whose
// container entries happen to match its bytes: the second Format applies.
func TestAPI_JSONValueNotEqualToLookalike(t *testing.T) {
	d := ygo.NewDoc()
	txt := ygo.NewText(d, "t")
	w := d.WriteTxn()
	if err := txt.Insert(w, 0, "x"); err != nil {
		t.Fatal(err)
	}
	if err := txt.Format(w, 0, 1, ygo.Attrs{"meta": int16(1)}); err != nil {
		t.Fatal(err)
	}
	w.Commit()
	w = d.WriteTxn()
	if err := txt.Format(w, 0, 1, ygo.Attrs{"meta": map[string]any{"0": 49}}); err != nil {
		t.Fatal(err)
	}
	w.Commit()
	got := txt.ToDelta()
	if len(got) != 1 || !reflect.DeepEqual(got[0].Attributes["meta"], map[string]any{"0": 49}) {
		t.Fatalf("delta %#v, want meta set to the map", got)
	}
}
