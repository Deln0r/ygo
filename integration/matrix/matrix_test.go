package matrix_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	ygo "github.com/Deln0r/ygo"
	ymatrix "github.com/Deln0r/ygo/integration/matrix"
)

const testRoom = id.RoomID("!doc:localhost")

func textOf(t *testing.T, d *ygo.Doc) string {
	t.Helper()
	txt := ygo.NewText(d, "t")
	rt := d.ReadTxn()
	defer rt.Close()
	return txt.String()
}

// peer builds a document with one text edit and returns doc + its transport.
func peer(t *testing.T, f *fakeHS, clientID uint64, at int, s string) (*ygo.Doc, *ymatrix.Transport) {
	t.Helper()
	d := ygo.NewDocWithOptions(ygo.Options{ClientID: clientID})
	txt := ygo.NewText(d, "t")
	txn := d.WriteTxn()
	if err := txt.Insert(txn, uint64(at), s); err != nil {
		t.Fatal(err)
	}
	txn.Commit()
	tr, err := ymatrix.New(f.client(t), testRoom)
	if err != nil {
		t.Fatal(err)
	}
	return d, tr
}

// TestFake_RejectsBadTokens keeps the double honest. It must refuse a
// malformed token (Dendrite does, asserted in TestDendrite_TokenHandling) and,
// by house rule, an empty one too. If the double ever accepts either, every
// pagination test below is worthless: the client could be reading one page and
// calling it the whole room.
func TestFake_RejectsBadTokens(t *testing.T) {
	f := newFakeHS(t)
	for _, tok := range []string{"", "garbage"} {
		_, err := f.client(t).Messages(context.Background(), testRoom, tok, "", mautrix.DirectionBackward, nil, 10)
		if err == nil {
			t.Fatalf("double accepted from=%q", tok)
		}
		if !strings.Contains(err.Error(), "M_INVALID_PARAM") {
			t.Fatalf("from=%q: unexpected error %v", tok, err)
		}
	}
}

// TestSync_NeverPaginatesFromEmptyToken pins the client behaviour the double
// now guards: Sync must reach history via prev_batch, never an empty token.
func TestSync_NeverPaginatesFromEmptyToken(t *testing.T) {
	f := newFakeHS(t)
	f.syncWindow = 1
	a, ta := peer(t, f, 1, 0, "alpha")
	if _, err := ta.PublishDoc(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	b, tb := peer(t, f, 2, 0, "beta")
	if _, err := tb.PublishDoc(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	// A third peer reads everything. With syncWindow=1 the older event is
	// only reachable by pagination, so a broken token would fail here.
	reader := ygo.NewDoc()
	tr, _ := ymatrix.New(f.client(t), testRoom)
	res, err := tr.Sync(context.Background(), reader)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if res.Applied != 2 {
		t.Fatalf("applied=%d skipped=%d, want 2 applied (history must be paginated, not dropped)", res.Applied, res.Skipped)
	}
	if got := textOf(t, reader); len(got) != len("alphabeta") {
		t.Fatalf("reader text %q lost an edit", got)
	}
}

// TestSync_DeduplicatesAcrossOverlap: the /sync window and the first backward
// page overlap on some servers. The duplicate must not be counted twice - and
// crucially the test uses TWO DIFFERENT peers, so a dedup key coarser than
// the event ID (say, per-room or per-sender) would pass a one-sided duplicate
// test while silently dropping the second peer's data.
func TestSync_DeduplicatesAcrossOverlap(t *testing.T) {
	f := newFakeHS(t)
	f.syncWindow = 1
	f.overlap = true

	a, ta := peer(t, f, 1, 0, "alpha")
	if _, err := ta.PublishDoc(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	b, tb := peer(t, f, 2, 0, "beta")
	if _, err := tb.PublishDoc(context.Background(), b); err != nil {
		t.Fatal(err)
	}

	reader := ygo.NewDoc()
	tr, _ := ymatrix.New(f.client(t), testRoom)
	res, err := tr.Sync(context.Background(), reader)
	if err != nil {
		t.Fatal(err)
	}
	if res.Applied != 2 {
		t.Fatalf("applied=%d, want exactly 2 (overlap must be deduplicated, both peers must survive)", res.Applied)
	}
	got := textOf(t, reader)
	if len(got) != len("alphabeta") {
		t.Fatalf("converged text %q; both peers' edits must be present", got)
	}
	// A second Sync re-reads the whole room (nothing is remembered between
	// calls) and must leave the document exactly as it was. Applied counts
	// the room, not the news - that is what its doc comment promises, and
	// state-vector equality is what actually proves nothing changed.
	svBefore := ygo.EncodeStateVector(reader)
	res2, err := tr.Sync(context.Background(), reader)
	if err != nil {
		t.Fatal(err)
	}
	if res2.Applied != 2 {
		t.Fatalf("re-sync applied=%d, want 2 (the room still holds two events)", res2.Applied)
	}
	if got2 := textOf(t, reader); got2 != got {
		t.Fatalf("re-sync changed the document: %q -> %q", got, got2)
	}
	if !bytes.Equal(svBefore, ygo.EncodeStateVector(reader)) {
		t.Fatal("re-sync moved the state vector; re-reading the room must be a no-op")
	}
}

// TestSync_ConvergesOutOfOrder: two peers edit in isolation and publish in
// the "wrong" order; both converge on the same non-empty state.
func TestSync_ConvergesOutOfOrder(t *testing.T) {
	f := newFakeHS(t)
	f.syncWindow = 2
	ctx := context.Background()

	a, ta := peer(t, f, 1, 0, "alpha")
	b, tb := peer(t, f, 2, 0, "beta")
	if _, err := tb.PublishDoc(ctx, b); err != nil { // second peer publishes first
		t.Fatal(err)
	}
	if _, err := ta.PublishDoc(ctx, a); err != nil {
		t.Fatal(err)
	}
	if _, err := ta.Sync(ctx, a); err != nil {
		t.Fatal(err)
	}
	if _, err := tb.Sync(ctx, b); err != nil {
		t.Fatal(err)
	}
	ga, gb := textOf(t, a), textOf(t, b)
	if ga != gb {
		t.Fatalf("peers diverged: %q vs %q", ga, gb)
	}
	if len(ga) != len("alphabeta") {
		t.Fatalf("converged on %q; convergence on a truncated document proves nothing", ga)
	}
}

// TestPublish_RejectsBadInput: a corrupt local export must fail for its own
// producer, not become every reader's problem.
func TestPublish_RejectsBadInput(t *testing.T) {
	f := newFakeHS(t)
	tr, _ := ymatrix.New(f.client(t), testRoom)
	ctx := context.Background()

	if _, err := tr.Publish(ctx, nil); err == nil {
		t.Error("published an empty update")
	}
	if _, err := tr.Publish(ctx, []byte{0xff, 0xff, 0xff, 0xff, 0xff}); err == nil {
		t.Error("published bytes that are not a valid update")
	}
	if n := len(f.snapshot()); n != 0 {
		t.Fatalf("%d event(s) reached the room; invalid updates must not be sent", n)
	}
}

// TestSync_SkipsHostileEvents: room content is untrusted. Unknown format,
// undecodable base64 and well-encoded garbage are each skipped, and a valid
// event published alongside them still lands.
func TestSync_SkipsHostileEvents(t *testing.T) {
	f := newFakeHS(t)
	f.syncWindow = 10
	ctx := context.Background()

	good, tg := peer(t, f, 1, 0, "alpha")
	if _, err := tg.PublishDoc(ctx, good); err != nil {
		t.Fatal(err)
	}
	bad := []map[string]any{
		{"format": "yjs-v99", "payload": base64.StdEncoding.EncodeToString([]byte("x"))},
		{"format": ymatrix.FormatV1, "payload": "!!!not base64!!!"},
		{"format": ymatrix.FormatV1, "payload": base64.StdEncoding.EncodeToString([]byte{0xff, 0xff, 0xff, 0xff})},
		{"format": ymatrix.FormatV1},
		{"nonsense": true},
	}
	for i, c := range bad {
		f.append(&event.Event{
			ID:      id.EventID(fmt.Sprintf("$bad%d", i)),
			Type:    ymatrix.EventType,
			RoomID:  testRoom,
			Content: event.Content{Raw: c},
		})
	}

	reader := ygo.NewDoc()
	tr, _ := ymatrix.New(f.client(t), testRoom)
	res, err := tr.Sync(ctx, reader)
	if err != nil {
		t.Fatalf("one hostile event must not fail the sync: %v", err)
	}
	if res.Applied != 1 {
		t.Fatalf("applied=%d, want 1 (the single valid update)", res.Applied)
	}
	if res.Skipped != len(bad) {
		t.Fatalf("skipped=%d, want %d", res.Skipped, len(bad))
	}
	if got := textOf(t, reader); got != "alpha" {
		t.Fatalf("reader text = %q, want %q", got, "alpha")
	}
}

// TestSync_IgnoresForeignEventTypes: a room carrying ordinary chat traffic is
// still usable; non-ygo events are not even counted as skipped.
func TestSync_IgnoresForeignEventTypes(t *testing.T) {
	f := newFakeHS(t)
	f.syncWindow = 10
	f.append(&event.Event{
		ID:      id.EventID("$chat1"),
		Type:    event.Type{Type: "m.room.message", Class: event.MessageEventType},
		RoomID:  testRoom,
		Content: event.Content{Raw: map[string]any{"body": "hello humans"}},
	})
	reader := ygo.NewDoc()
	tr, _ := ymatrix.New(f.client(t), testRoom)
	res, err := tr.Sync(context.Background(), reader)
	if err != nil {
		t.Fatal(err)
	}
	if res.Applied != 0 || res.Skipped != 0 {
		t.Fatalf("chat traffic disturbed the sync: %+v", res)
	}
}

// TestSync_EmptyPageIsNotTheEndOfHistory: the spec ends /messages pagination
// by omitting `end`, not by an empty chunk. A server may legally hand back a
// page with no events and a token that leads to more. Treating the empty
// chunk as the end drops everything older AND still returns success, which is
// the shape of bug that never gets noticed.
func TestSync_EmptyPageIsNotTheEndOfHistory(t *testing.T) {
	f := newFakeHS(t)
	f.syncWindow = 1
	ctx := context.Background()

	a, ta := peer(t, f, 1, 0, "alpha")
	if _, err := ta.PublishDoc(ctx, a); err != nil {
		t.Fatal(err)
	}
	b, tb := peer(t, f, 2, 0, "beta")
	if _, err := tb.PublishDoc(ctx, b); err != nil {
		t.Fatal(err)
	}
	c, tc := peer(t, f, 3, 0, "gamma")
	if _, err := tc.PublishDoc(ctx, c); err != nil {
		t.Fatal(err)
	}
	// The page reached after the newest event comes back empty; the two older
	// events live past it.
	f.emptyPageAt = 2

	reader := ygo.NewDoc()
	tr, _ := ymatrix.New(f.client(t), testRoom)
	res, err := tr.Sync(ctx, reader)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if res.Applied != 3 {
		t.Fatalf("applied=%d, want 3: an empty page in the middle of history truncated the read", res.Applied)
	}
	if got := textOf(t, reader); len(got) != len("alphabetagamma") {
		t.Fatalf("reader text %q is missing an edit", got)
	}
}

// TestSync_PaginatesEveryPage: with a one-event page size, reading four
// events must cost several /messages calls. Without this the pagination loop
// could stop after the first page and every other test would still pass,
// because a double that returns the whole room in one page cannot tell the
// two behaviours apart.
func TestSync_PaginatesEveryPage(t *testing.T) {
	f := newFakeHS(t)
	f.syncWindow = 1
	f.pageSize = 1
	ctx := context.Background()

	for i, s := range []string{"aa", "bb", "cc", "dd"} {
		d, tr := peer(t, f, uint64(i+1), 0, s)
		if _, err := tr.PublishDoc(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	reader := ygo.NewDoc()
	tr, _ := ymatrix.New(f.client(t), testRoom)
	res, err := tr.Sync(ctx, reader)
	if err != nil {
		t.Fatal(err)
	}
	if res.Applied != 4 {
		t.Fatalf("applied=%d, want 4", res.Applied)
	}
	if f.msgCalls < 3 {
		t.Fatalf("%d /messages call(s) for 3 paginated events: the client is not walking pages", f.msgCalls)
	}
	if got := textOf(t, reader); len(got) != 8 {
		t.Fatalf("reader text %q, want all four edits", got)
	}
}

// TestSync_RefusesTokenLoop: a malformed server that re-issues a token it has
// already handed out must produce an error, not an endless loop. Comparing
// only against the immediately previous token is not enough - this server
// alternates.
func TestSync_RefusesTokenLoop(t *testing.T) {
	f := newFakeHS(t)
	f.syncWindow = 1
	f.pageSize = 1
	ctx := context.Background()

	for i, s := range []string{"aa", "bb", "cc"} {
		d, tr := peer(t, f, uint64(i+1), 0, s)
		if _, err := tr.PublishDoc(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	f.cycleAfter = 1

	reader := ygo.NewDoc()
	tr, _ := ymatrix.New(f.client(t), testRoom)
	_, err := tr.Sync(ctx, reader)
	if err == nil {
		t.Fatal("sync accepted a server that re-issues pagination tokens; on a real one this never returns")
	}
	if !strings.Contains(err.Error(), "already issued") {
		t.Fatalf("unexpected error %v", err)
	}
}

// TestSync_UnreachableRoomIsAnError: a room this account cannot see is absent
// from /sync. An initial sync lists joined rooms even when they are empty
// (measured against Dendrite, 2026-09-03), so absence means "not joined or
// wrong ID" - and reporting an empty success would make a typo in a room ID
// indistinguishable from a healthy, quiet document.
func TestSync_UnreachableRoomIsAnError(t *testing.T) {
	f := newFakeHS(t)
	f.roomMissing = true
	tr, _ := ymatrix.New(f.client(t), testRoom)
	res, err := tr.Sync(context.Background(), ygo.NewDoc())
	if err == nil {
		t.Fatalf("sync on an unreachable room returned success: %+v", res)
	}
	if !errors.Is(err, ymatrix.ErrRoomUnavailable) {
		t.Fatalf("unexpected error %v", err)
	}
}

// TestEncryptedRoomIsRefused: this transport does not implement Megolm.
// Publishing into an encrypted room would put document contents in the clear
// in a room whose members were promised otherwise - and the server accepts it
// without complaint (measured against Dendrite, 2026-09-03). Reading one is
// broken the other way: every real event arrives as m.room.encrypted, this
// transport skips them all, and the room looks serenely empty.
func TestEncryptedRoomIsRefused(t *testing.T) {
	f := newFakeHS(t)
	f.encrypted = true
	ctx := context.Background()

	d, tr := peer(t, f, 1, 0, "alpha")
	if _, err := tr.PublishDoc(ctx, d); !errors.Is(err, ymatrix.ErrRoomEncrypted) {
		t.Fatalf("publish into an encrypted room: err=%v, want ErrRoomEncrypted", err)
	}
	if n := len(f.snapshot()); n != 0 {
		t.Fatalf("%d event(s) reached an encrypted room in the clear", n)
	}
	tr2, _ := ymatrix.New(f.client(t), testRoom)
	if _, err := tr2.Sync(ctx, ygo.NewDoc()); !errors.Is(err, ymatrix.ErrRoomEncrypted) {
		t.Fatalf("sync on an encrypted room: err=%v, want ErrRoomEncrypted", err)
	}
}

// TestPublish_RejectsConcatenatedUpdates: appending one update to another is
// a natural-looking mistake that loses the second half in total silence -
// ApplyUpdate reads the first update and ignores the rest, and so does yjs.
// The publisher is where that has to be caught.
func TestPublish_RejectsConcatenatedUpdates(t *testing.T) {
	f := newFakeHS(t)
	tr, _ := ymatrix.New(f.client(t), testRoom)
	ctx := context.Background()

	a, _ := peer(t, f, 1, 0, "alpha")
	b, _ := peer(t, f, 2, 0, "beta")
	ua, ub := ygo.EncodeStateAsUpdate(a), ygo.EncodeStateAsUpdate(b)

	if _, err := tr.Publish(ctx, append(append([]byte(nil), ua...), ub...)); err == nil {
		t.Fatal("published two concatenated updates; the second one would have been silently dropped by every reader")
	}
	// The supported way to combine them goes through.
	merged, err := ygo.MergeUpdates([][]byte{ua, ub})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Publish(ctx, merged); err != nil {
		t.Fatalf("merged update refused: %v", err)
	}
}

// TestOversizeUpdateIsRefusedBothWays: Matrix caps a complete event at 65536
// bytes and base64 costs a third on top. An update over the limit must fail
// locally with something readable rather than as an opaque server rejection,
// and an oversized payload in the room must be skipped before it is decoded.
func TestOversizeUpdateIsRefusedBothWays(t *testing.T) {
	f := newFakeHS(t)
	f.syncWindow = 10
	ctx := context.Background()

	big := ygo.NewDocWithOptions(ygo.Options{ClientID: 1})
	txt := ygo.NewText(big, "t")
	txn := big.WriteTxn()
	if err := txt.Insert(txn, 0, strings.Repeat("x", ymatrix.MaxUpdateBytes+1000)); err != nil {
		t.Fatal(err)
	}
	txn.Commit()
	update := ygo.EncodeStateAsUpdate(big)
	if len(update) <= ymatrix.MaxUpdateBytes {
		t.Fatalf("test setup: update is only %d bytes, not over the limit", len(update))
	}

	tr, _ := ymatrix.New(f.client(t), testRoom)
	if _, err := tr.Publish(ctx, update); err == nil {
		t.Fatal("published an update too large for a Matrix event")
	}
	if n := len(f.snapshot()); n != 0 {
		t.Fatalf("%d oversized event(s) reached the room", n)
	}

	// Same payload planted directly in the room by a peer that does not check.
	f.append(&event.Event{
		ID:      id.EventID("$oversize"),
		Type:    ymatrix.EventType,
		RoomID:  testRoom,
		Content: event.Content{Raw: map[string]any{"format": ymatrix.FormatV1, "payload": base64.StdEncoding.EncodeToString(update)}},
	})
	res, err := tr.Sync(ctx, ygo.NewDoc())
	if err != nil {
		t.Fatal(err)
	}
	if res.Applied != 0 || res.Skipped != 1 {
		t.Fatalf("oversized room event: applied=%d skipped=%d, want 0/1", res.Applied, res.Skipped)
	}
}

// TestSync_SecondDocumentGetsFullHistory: Transport holds no document, and its
// doc comment says one Transport can serve more than one. A dedup set kept
// across calls would make this pass for the first document and hand the second
// an empty one - a data loss that only shows up when someone takes the API at
// its word.
func TestSync_SecondDocumentGetsFullHistory(t *testing.T) {
	f := newFakeHS(t)
	f.syncWindow = 1
	ctx := context.Background()

	a, ta := peer(t, f, 1, 0, "alpha")
	if _, err := ta.PublishDoc(ctx, a); err != nil {
		t.Fatal(err)
	}
	b, tb := peer(t, f, 2, 0, "beta")
	if _, err := tb.PublishDoc(ctx, b); err != nil {
		t.Fatal(err)
	}

	tr, _ := ymatrix.New(f.client(t), testRoom)
	first, second := ygo.NewDoc(), ygo.NewDoc()
	if _, err := tr.Sync(ctx, first); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Sync(ctx, second); err != nil {
		t.Fatal(err)
	}
	g1, g2 := textOf(t, first), textOf(t, second)
	if g1 != g2 || len(g2) != len("alphabeta") {
		t.Fatalf("second document got %q, first got %q; both must see the whole room", g2, g1)
	}
}

// TestSync_HonoursContextCancellation: integrating an update is superlinear
// in the conflicts it carries, so one page of hostile-but-legal events is slow
// to merge. That cost cannot be removed here - it is YATA's, and the reference
// implementation shares it - but a caller's deadline has to survive it.
//
// The cancellation deliberately lands mid-merge rather than before the call.
// Cancelling up front proves nothing: the HTTP client fails first and the test
// passes with no guard in the loop at all (measured - that is exactly how the
// first version of this test passed against a deliberately broken build). The
// fake serves the whole room in one /sync window and hands out no back-token,
// so once the response is parsed there is no network left; a cancellation
// observed after that can only have come from the merge loop, and the error
// message is asserted to say so.
func TestSync_HonoursContextCancellation(t *testing.T) {
	f := newFakeHS(t)
	f.syncWindow = 100

	// One update carrying many single-item runs that all conflict at the same
	// position: the expensive shape, published a few times over.
	slow := conflictHeavyUpdate(t, 1500)
	tr, _ := ymatrix.New(f.client(t), testRoom)
	for i := 0; i < 4; i++ {
		f.append(&event.Event{
			ID:      id.EventID(fmt.Sprintf("$slow%d", i)),
			Type:    ymatrix.EventType,
			RoomID:  testRoom,
			Content: event.Content{Raw: map[string]any{"format": ymatrix.FormatV1, "payload": base64.StdEncoding.EncodeToString(slow)}},
		})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := tr.Sync(ctx, ygo.NewDoc())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("sync err=%v, want context.DeadlineExceeded after %s", err, time.Since(start))
	}
	if !strings.Contains(err.Error(), "merging room history") {
		t.Fatalf("cancellation came from %v, not from the merge loop; the test is not exercising the guard", err)
	}
}

// conflictHeavyUpdate builds an update of n single-item runs from n distinct
// clients, all inserting at the same position, so every item conflicts with
// every earlier one. Merging it is quadratic by construction.
func conflictHeavyUpdate(t *testing.T, n int) []byte {
	t.Helper()
	ups := make([][]byte, 0, n)
	for i := 1; i <= n; i++ {
		d := ygo.NewDocWithOptions(ygo.Options{ClientID: uint64(i)})
		txt := ygo.NewText(d, "t")
		txn := d.WriteTxn()
		if err := txt.Insert(txn, 0, "x"); err != nil {
			t.Fatal(err)
		}
		txn.Commit()
		ups = append(ups, ygo.EncodeStateAsUpdate(d))
	}
	m, err := ygo.MergeUpdates(ups)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// TestSync_SurvivesUndecodableEvent is the module's central untrusted-input
// claim, stated as a test: one bad publisher must not deny the room to
// everybody else.
//
// The hostile event here is not malformed JSON. It is a well-formed Matrix
// event whose `content` is a string rather than an object. mautrix decodes a
// whole /sync or /messages response as one typed tree, so a single such event
// makes the entire page fail to unmarshal, taking every legitimate update in
// that page with it. Decoding events one at a time is the difference between
// skipping one event and losing the room.
//
// Reachability, measured and NOT overstated: Dendrite's own /send rejects a
// non-object content with M_BAD_JSON, so a member of a Dendrite room cannot
// post this (TestDendrite_HostileEventInRealRoom records that by skipping).
// It arrives from a different server implementation, over federation, or from
// a server-side defect - rare, and catastrophic when the answer is "lose the
// whole page", which is why the client tolerates it rather than trusting the
// server to be strict.
func TestSync_SurvivesUndecodableEvent(t *testing.T) {
	f := newFakeHS(t)
	f.syncWindow = 10
	ctx := context.Background()

	a, ta := peer(t, f, 1, 0, "alpha")
	if _, err := ta.PublishDoc(ctx, a); err != nil {
		t.Fatal(err)
	}
	f.appendRaw(json.RawMessage(`{"event_id":"$poison","type":"dev.ygo.update","room_id":"!doc:localhost","sender":"@x:localhost","origin_server_ts":1,"content":"i am a string, not an object"}`))
	b, tb := peer(t, f, 2, 0, "beta")
	if _, err := tb.PublishDoc(ctx, b); err != nil {
		t.Fatal(err)
	}

	reader := ygo.NewDoc()
	tr, _ := ymatrix.New(f.client(t), testRoom)
	res, err := tr.Sync(ctx, reader)
	if err != nil {
		t.Fatalf("one undecodable event failed the whole sync: %v", err)
	}
	if res.Applied != 2 {
		t.Fatalf("applied=%d skipped=%d, want 2 applied: the good events either side of the poison must survive", res.Applied, res.Skipped)
	}
	if got := textOf(t, reader); len(got) != len("alphabeta") {
		t.Fatalf("reader text %q; one hostile event cost a legitimate edit", got)
	}
}

// TestSync_SurvivesUndecodableEventInHistory: same, but the poison sits in a
// paginated page rather than the /sync window, because those are decoded by a
// different call.
func TestSync_SurvivesUndecodableEventInHistory(t *testing.T) {
	f := newFakeHS(t)
	f.syncWindow = 1
	f.pageSize = 3
	ctx := context.Background()

	a, ta := peer(t, f, 1, 0, "alpha")
	if _, err := ta.PublishDoc(ctx, a); err != nil {
		t.Fatal(err)
	}
	f.appendRaw(json.RawMessage(`{"event_id":"$poison2","type":"dev.ygo.update","room_id":"!doc:localhost","sender":"@x:localhost","origin_server_ts":1,"content":7}`))
	b, tb := peer(t, f, 2, 0, "beta")
	if _, err := tb.PublishDoc(ctx, b); err != nil {
		t.Fatal(err)
	}
	c, tc := peer(t, f, 3, 0, "gamma")
	if _, err := tc.PublishDoc(ctx, c); err != nil {
		t.Fatal(err)
	}

	reader := ygo.NewDoc()
	tr, _ := ymatrix.New(f.client(t), testRoom)
	res, err := tr.Sync(ctx, reader)
	if err != nil {
		t.Fatalf("poison in a history page failed the sync: %v", err)
	}
	if res.Applied != 3 {
		t.Fatalf("applied=%d skipped=%d, want 3", res.Applied, res.Skipped)
	}
	if got := textOf(t, reader); len(got) != len("alphabetagamma") {
		t.Fatalf("reader text %q lost an edit to the poison event", got)
	}
}
