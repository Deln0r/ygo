package ygo_test

import (
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/Deln0r/ygo"
)

// An attribute value that is not equal to itself (NaN) is never "already in
// effect": yjs insertText writes an opening marker for it and a marker that
// restores it, even for an insert without attributes, which copies the
// attributes in effect. The format value travels as JSON, so NaN is written
// as null. Expected bytes are the 'update' events of yjs 13.6.33 for
// insert(0, 'ab', {n: NaN}) and then insert(1, 'X') on client 3141592653.
func TestText_InsertInheritsNaNAttribute(t *testing.T) {
	const (
		wantFirst  = "0103cdcc83da0b0006010174016e046e756c6c84cdcc83da0b0002616286cdcc83da0b02016e046e756c6c00"
		wantSecond = "0103cdcc83da0b04c6cdcc83da0b01cdcc83da0b02016e046e756c6cc4cdcc83da0b04cdcc83da0b020158c6cdcc83da0b05cdcc83da0b02016e046e756c6c00"
	)
	d := ygo.NewDocWithOptions(ygo.Options{ClientID: 3141592653})
	txt := ygo.NewText(d, "t")
	var updates []string
	d.OnUpdate(func(u []byte, _ any) { updates = append(updates, hex.EncodeToString(u)) })

	w := d.WriteTxn()
	if err := txt.InsertWithAttributes(w, 0, "ab", ygo.Attrs{"n": math.NaN()}); err != nil {
		t.Fatal(err)
	}
	w.Commit()
	w = d.WriteTxn()
	if err := txt.Insert(w, 1, "X"); err != nil {
		t.Fatal(err)
	}
	w.Commit()

	if len(updates) != 2 {
		t.Fatalf("got %d updates, want 2", len(updates))
	}
	if updates[0] != wantFirst {
		t.Errorf("first update\n got %s\nwant %s", updates[0], wantFirst)
	}
	if updates[1] != wantSecond {
		t.Errorf("second update\n got %s\nwant %s", updates[1], wantSecond)
	}
}

// JavaScript has one number type, so a Go int attribute equals the same
// number a peer wrote, which decodes as int64. The update-event scenario
// text-insert-number-attr-from-peer replays it with the int64 that JSON
// decoding gives; this replays its last step with a Go int literal and
// expects the same bytes: no format markers around the insert.
func TestText_InsertNumberAttributeAcrossGoKinds(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "update-event-fixtures.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Scenarios []struct {
			Name   string `json:"name"`
			Events []struct {
				Step int    `json:"step"`
				Doc  int    `json:"doc"`
				V1   string `json:"v1"`
			} `json:"events"`
			Steps []struct {
				Apply *struct {
					Update string `json:"update"`
				} `json:"apply"`
			} `json:"steps"`
		} `json:"scenarios"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	for _, sc := range f.Scenarios {
		if sc.Name != "text-insert-number-attr-from-peer" {
			continue
		}
		var peerState, want string
		for _, st := range sc.Steps {
			if st.Apply != nil {
				peerState = st.Apply.Update
			}
		}
		for _, ev := range sc.Events {
			if ev.Step == 2 && ev.Doc == 0 {
				want = ev.V1
			}
		}
		if peerState == "" || want == "" {
			t.Fatalf("scenario lacks the applied state or the last event")
		}
		raw, err := hex.DecodeString(peerState)
		if err != nil {
			t.Fatal(err)
		}
		d := ygo.NewDocWithOptions(ygo.Options{ClientID: 3141592653})
		txt := ygo.NewText(d, "t")
		if err := ygo.ApplyUpdate(d, raw); err != nil {
			t.Fatal(err)
		}
		var got string
		d.OnUpdate(func(u []byte, _ any) { got = hex.EncodeToString(u) })
		w := d.WriteTxn()
		if err := txt.InsertWithAttributes(w, 1, "X", ygo.Attrs{"level": 1}); err != nil {
			t.Fatal(err)
		}
		w.Commit()
		if got != want {
			t.Errorf("update\n got %s\nwant %s", got, want)
		}
		return
	}
	t.Fatal("scenario text-insert-number-attr-from-peer not found")
}
