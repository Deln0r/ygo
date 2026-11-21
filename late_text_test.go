package ygo_test

import (
	"fmt"
	"testing"

	"github.com/Deln0r/ygo"
	"github.com/Deln0r/ygo/internal/encoding"
)

// lateText returns a receiver whose root "t" received "abcd" with "cd" bold
// while untyped and was then typed, and the writer.
func lateText(t *testing.T) (writer *ygo.Doc, wt *ygo.Text, receiver *ygo.Doc, rt *ygo.Text) {
	t.Helper()
	writer = ygo.NewDocWithOptions(ygo.Options{ClientID: 3141592653})
	wt = ygo.NewText(writer, "t")
	txn := writer.WriteTxn()
	if err := wt.Insert(txn, 0, "abcd"); err != nil {
		t.Fatal(err)
	}
	txn.Commit()
	txn = writer.WriteTxn()
	if err := wt.Format(txn, 2, 2, ygo.Attrs{"bold": true}); err != nil {
		t.Fatal(err)
	}
	txn.Commit()
	receiver = ygo.NewDocWithOptions(ygo.Options{ClientID: 271828182})
	if err := ygo.ApplyUpdateWithOrigin(receiver, ygo.EncodeStateAsUpdate(writer), "remote"); err != nil {
		t.Fatal(err)
	}
	rt = ygo.NewText(receiver, "t")
	if !rt.Branch().LateText() {
		t.Fatal("the receiver's text is not in the late state")
	}
	return writer, wt, receiver, rt
}

func lateInsert(t *testing.T, d *ygo.Doc, x *ygo.Text, idx uint64, s string) {
	t.Helper()
	txn := d.WriteTxn()
	defer txn.Commit()
	if err := x.Insert(txn, idx, s); err != nil {
		t.Fatal(err)
	}
}

func lateDelta(x *ygo.Text) string {
	out := ""
	for _, op := range x.ToDelta() {
		out += fmt.Sprintf("%q%v ", op.Insert, op.Attributes)
	}
	return out
}

// An update applied in a transaction that then inserts: yjs clears a
// type's search markers when the commit reaches it (_callObserver), not
// when the update is applied, so the insert still starts at the marker the
// earlier insert left and is bold (yjs: transact(() => { applyUpdate(...);
// insert(2, "Z") })). ygo's API applies an update in a transaction of its
// own; this drives the internal path.
func TestLateText_ApplyAndInsertInOneRemoteTransaction(t *testing.T) {
	writer, wt, receiver, rt := lateText(t)
	lateInsert(t, receiver, rt, 3, "Q")
	lateInsert(t, writer, wt, 4, "e")
	diff, err := ygo.EncodeDiff(writer, ygo.EncodeStateVector(receiver))
	if err != nil {
		t.Fatal(err)
	}
	upd, _, err := encoding.DecodeUpdate(diff)
	if err != nil {
		t.Fatal(err)
	}
	txn := receiver.WriteTxn()
	txn.Local = false
	if err := upd.Apply(txn); err != nil {
		t.Fatal(err)
	}
	if err := rt.Insert(txn, 2, "Z"); err != nil {
		t.Fatal(err)
	}
	txn.Commit()
	// yjs 13.6.33: [{"insert":"ab"},{"insert":"ZcQde","attributes":{"bold":true}}]
	if got, want := lateDelta(rt), `"ab"map[] "ZcQde"map[bold:true] `; got != want {
		t.Fatalf("delta %s, want %s", got, want)
	}
	if n := rt.Branch().TextMarkers.Len(); n != 0 {
		t.Fatalf("%d markers after the commit, want none (the remote transaction clears them)", n)
	}
}

// A late text never fills the MarkerList, so when a format item ends the
// late state the formatted-text paths, which use it, start from an empty
// one.
func TestLateText_MarkerListEmptyWhenFormatted(t *testing.T) {
	_, _, receiver, rt := lateText(t)
	lateInsert(t, receiver, rt, 3, "Q")
	lateInsert(t, receiver, rt, 1, "P")
	txn := receiver.WriteTxn()
	if err := rt.Delete(txn, 0, 1); err != nil {
		t.Fatal(err)
	}
	if err := rt.InsertEmbed(txn, 2, map[string]any{"img": 1}); err != nil {
		t.Fatal(err)
	}
	txn.Commit()
	br := rt.Branch()
	if br.TextMarkers.Len() == 0 {
		t.Fatal("no text markers: the late path was not taken")
	}
	if n := br.Markers.Len(); n != 0 {
		t.Fatalf("the MarkerList holds %d markers in the late state", n)
	}
	txn = receiver.WriteTxn()
	if err := rt.Format(txn, 0, 1, ygo.Attrs{"italic": true}); err != nil {
		t.Fatal(err)
	}
	txn.Commit()
	if br.LateText() || br.TextMarkers != nil {
		t.Fatal("a local format did not end the late state")
	}
	if n := br.Markers.Len(); n != 0 {
		t.Fatalf("the MarkerList holds %d markers after the format", n)
	}
}

// A nested text is built typed before its items integrate, so a format item
// a peer sends marks it formatted: it is never in the late state.
func TestLateText_NestedTextIsNeverLate(t *testing.T) {
	writer := ygo.NewDocWithOptions(ygo.Options{ClientID: 1})
	m := ygo.NewMap(writer, "m")
	txn := writer.WriteTxn()
	inner := m.SetText(txn, "t")
	if err := inner.InsertWithAttributes(txn, 0, "xy", ygo.Attrs{"bold": true}); err != nil {
		t.Fatal(err)
	}
	txn.Commit()
	receiver := ygo.NewDocWithOptions(ygo.Options{ClientID: 2})
	if err := ygo.ApplyUpdate(receiver, ygo.EncodeStateAsUpdate(writer)); err != nil {
		t.Fatal(err)
	}
	got, ok := ygo.NewMap(receiver, "m").Get("t").(*ygo.Text)
	if !ok {
		t.Fatal("no nested text")
	}
	if br := got.Branch(); br.LateText() || !br.FormattedText {
		t.Fatalf("nested text: late %v, formatted %v", br.LateText(), br.FormattedText)
	}
}
