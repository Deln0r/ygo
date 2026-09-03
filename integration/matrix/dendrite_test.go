package matrix_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	ygo "github.com/Deln0r/ygo"
	ymatrix "github.com/Deln0r/ygo/integration/matrix"
)

// These run against a REAL Dendrite (testdata/docker-compose.yml). They are
// the reason the package can claim Matrix interoperability at all: the unit
// double is written by the same hand as the client, so only a real homeserver
// can refute an assumption both of them share. Skipped unless a homeserver is
// reachable, so `go test ./...` stays useful without Docker; CI always has one
// and therefore always runs them.
const homeserverURL = "http://localhost:8008"

func requireHomeserver(t *testing.T) {
	t.Helper()
	if os.Getenv("YGO_MATRIX_SKIP_IT") != "" {
		t.Skip("YGO_MATRIX_SKIP_IT set")
	}
	c := &http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get(homeserverURL + "/_matrix/client/versions")
	if err != nil {
		t.Skipf("no homeserver at %s (run testdata/up.sh): %v", homeserverURL, err)
	}
	resp.Body.Close()
}

// register creates a fresh account and returns a logged-in client. Open
// registration is enabled by the compose stack for exactly this.
func register(t *testing.T, localpart string) *mautrix.Client {
	t.Helper()
	cli, err := mautrix.NewClient(homeserverURL, "", "")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	resp, err := cli.RegisterDummy(ctx, &mautrix.ReqRegister[any]{
		Username: localpart,
		Password: "correct-horse-battery-staple",
	})
	if err != nil {
		t.Fatalf("register %s: %v", localpart, err)
	}
	out, err := mautrix.NewClient(homeserverURL, resp.UserID, resp.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func uniq(prefix string) string {
	return fmt.Sprintf("%s%d", prefix, time.Now().UnixNano())
}

// syncFresh syncs into a NEW document on every attempt until it holds
// wantLen characters, or the deadline passes, and returns what it read.
//
// The freshness is the point. Matrix is eventually consistent, so a
// just-published event is not guaranteed to be in the very next /sync and a
// real peer polls rather than assuming one round trip suffices - but polling
// INTO THE SAME document accumulates, and accumulation hides the bug the
// polling is meant to tolerate. If call one exposed E1 and call two exposed
// E2, a reused document reaches the target while no single Sync ever read the
// whole room, which is exactly what a broken pagination loop looks like. With
// a fresh document each time, success means one Sync read everything.
func syncFresh(t *testing.T, tr *ymatrix.Transport, wantLen int) (*ygo.Doc, string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		doc := ygo.NewDoc()
		if _, err := tr.Sync(context.Background(), doc); err != nil {
			// A room joined a moment ago is not guaranteed to be in the very
			// next /sync, and the transport correctly calls a room it cannot
			// see unavailable. That is a state to poll through, not a
			// failure - but only until the deadline, so a genuinely wrong
			// room ID still fails loudly.
			if errors.Is(err, ymatrix.ErrRoomUnavailable) && time.Now().Before(deadline) {
				time.Sleep(100 * time.Millisecond)
				continue
			}
			t.Fatalf("sync: %v", err)
		}
		got := textOf(t, doc)
		if len(got) >= wantLen || time.Now().After(deadline) {
			return doc, got
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// sharedHistory makes a room readable from before a peer joined, explicitly
// rather than by relying on a preset default. A room set to
// history_visibility=joined hands a newcomer nothing that predates their
// membership, and the document they reconstruct is silently partial - the
// room is only an append-only log for the people allowed to read it.
func sharedHistory() []*event.Event {
	empty := ""
	return []*event.Event{{
		Type:     event.StateHistoryVisibility,
		StateKey: &empty,
		Content:  event.Content{Raw: map[string]any{"history_visibility": "shared"}},
	}}
}

// TestDendrite_TwoPeersConverge is the acceptance scenario: two peers edit
// while they cannot see each other, publish in the inconvenient order (the
// second one publishes first), then read the room back and converge. The
// assertions deliberately include "the document is not empty" and "both edits
// survived" - convergence on an empty document is also convergence, and
// proves nothing.
func TestDendrite_TwoPeersConverge(t *testing.T) {
	requireHomeserver(t)
	ctx := context.Background()

	alice := register(t, uniq("alice"))
	bob := register(t, uniq("bob"))

	created, err := alice.CreateRoom(ctx, &mautrix.ReqCreateRoom{
		Preset:       "public_chat",
		Name:         "ygo doc",
		InitialState: sharedHistory(),
	})
	if err != nil {
		t.Fatalf("create room: %v", err)
	}
	room := created.RoomID
	if _, err := bob.JoinRoomByID(ctx, room); err != nil {
		t.Fatalf("bob join: %v", err)
	}

	// Two documents edited in isolation.
	da := ygo.NewDocWithOptions(ygo.Options{ClientID: 111})
	ta := ygo.NewText(da, "t")
	txn := da.WriteTxn()
	if err := ta.Insert(txn, 0, "alpha"); err != nil {
		t.Fatal(err)
	}
	txn.Commit()

	db := ygo.NewDocWithOptions(ygo.Options{ClientID: 222})
	tb := ygo.NewText(db, "t")
	txn = db.WriteTxn()
	if err := tb.Insert(txn, 0, "beta"); err != nil {
		t.Fatal(err)
	}
	txn.Commit()

	tra, err := ymatrix.New(alice, room)
	if err != nil {
		t.Fatal(err)
	}
	trb, err := ymatrix.New(bob, room)
	if err != nil {
		t.Fatal(err)
	}

	// Inconvenient order on purpose: the second peer publishes first.
	if _, err := trb.PublishDoc(ctx, db); err != nil {
		t.Fatalf("bob publish: %v", err)
	}
	if _, err := tra.PublishDoc(ctx, da); err != nil {
		t.Fatalf("alice publish: %v", err)
	}

	_, ga := syncFresh(t, tra, len("alphabeta"))
	_, gb := syncFresh(t, trb, len("alphabeta"))
	if ga != gb {
		t.Fatalf("peers diverged over a real homeserver: %q vs %q", ga, gb)
	}
	if ga == "" {
		t.Fatal("both peers converged on an empty document; that proves nothing")
	}
	if len(ga) != len("alphabeta") {
		t.Fatalf("converged on %q; both edits must survive", ga)
	}
}

// TestDendrite_TokenHandling asserts what the real server does with
// pagination tokens, because the unit double imitates it and a stale
// imitation is worse than none.
//
// MEASURED, not assumed, on 2026-09-03: Dendrite REJECTS a malformed token
// with 400 M_INVALID_PARAM, and ACCEPTS a literal empty one with 200. The
// accepting case is the dangerous one - it looks like success while returning
// a single page - which is why Sync pages from the /sync prev_batch token
// instead, and why the double refuses empty tokens outright.
//
// The empty case goes over raw HTTP on purpose: every client library here,
// mautrix included, drops a `from` it considers unset, so calling through one
// tests the library's omission rather than the server's handling of an empty
// value. That is how the first version of this test ended up asserting
// nothing at all.
func TestDendrite_TokenHandling(t *testing.T) {
	requireHomeserver(t)
	ctx := context.Background()
	alice := register(t, uniq("tok"))
	created, err := alice.CreateRoom(ctx, &mautrix.ReqCreateRoom{Preset: "public_chat"})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := alice.Messages(ctx, created.RoomID, "garbage", "", mautrix.DirectionBackward, nil, 10); err == nil {
		t.Fatal("Dendrite accepted a malformed from-token; the double now imitates behaviour the server no longer has")
	} else if !strings.Contains(err.Error(), "M_INVALID_PARAM") {
		t.Fatalf("malformed token rejected with an unexpected error: %v", err)
	}

	status, body := rawGET(t, alice, fmt.Sprintf("/_matrix/client/v3/rooms/%s/messages?dir=b&limit=10&from=",
		url.PathEscape(string(created.RoomID))))
	if status != http.StatusOK {
		t.Fatalf("Dendrite answered %d to a literal empty from-token (body %s).\n"+
			"This reverses the measurement the double and the README are built on. "+
			"Neither the transport nor its safety depends on the old answer - Sync never sends an empty token - "+
			"but the comments claiming Dendrite accepts one are now wrong and must be corrected.", status, body)
	}
	if !strings.Contains(body, "\"chunk\"") {
		t.Fatalf("empty from-token returned 200 without a chunk: %s", body)
	}
}

// rawGET issues an authenticated request without going through a client
// library, so a query parameter reaches the server exactly as written.
func rawGET(t *testing.T, cli *mautrix.Client, path string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, homeserverURL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+cli.AccessToken)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(b)
}

// TestDendrite_ThirdPeerReadsHistory: a peer that was never present joins and
// reconstructs the document from the room alone - the room is the log, and no
// ygo server is involved anywhere.
//
// This is client-to-server behaviour against one homeserver, not federation:
// the compose stack runs a single Dendrite, and nothing in this suite crosses
// a server boundary. What it does establish is the property federation would
// carry - that a peer needs the room and nothing else - which is why the room
// is created with history_visibility=shared explicitly. Set to `joined`, the
// same room hands a newcomer only what was posted after they arrived, and the
// document they rebuild is quietly incomplete.
func TestDendrite_ThirdPeerReadsHistory(t *testing.T) {
	requireHomeserver(t)
	ctx := context.Background()

	author := register(t, uniq("author"))
	created, err := author.CreateRoom(ctx, &mautrix.ReqCreateRoom{Preset: "public_chat", InitialState: sharedHistory()})
	if err != nil {
		t.Fatal(err)
	}
	room := created.RoomID

	tr, _ := ymatrix.New(author, room)
	for i, word := range []string{"one", "two", "three"} {
		d := ygo.NewDocWithOptions(ygo.Options{ClientID: uint64(500 + i)})
		txt := ygo.NewText(d, "t")
		txn := d.WriteTxn()
		if err := txt.Insert(txn, 0, word); err != nil {
			t.Fatal(err)
		}
		txn.Commit()
		if _, err := tr.PublishDoc(ctx, d); err != nil {
			t.Fatalf("publish %s: %v", word, err)
		}
	}

	newcomer := register(t, uniq("newcomer"))
	if _, err := newcomer.JoinRoomByID(ctx, room); err != nil {
		t.Fatal(err)
	}
	trn, _ := ymatrix.New(newcomer, room)
	doc, got := syncFresh(t, trn, len("onetwothree"))
	if len(got) != len("onetwothree") {
		t.Fatalf("newcomer reconstructed %q; all three edits must be present", got)
	}
	// Re-syncing the same document must change nothing. Applied counts what
	// the room holds rather than what is new, so the honest assertion is on
	// the document: the state vector, which cannot move if nothing was added.
	svBefore := ygo.EncodeStateVector(doc)
	if _, err := trn.Sync(ctx, doc); err != nil {
		t.Fatal(err)
	}
	if textOf(t, doc) != got || !bytes.Equal(svBefore, ygo.EncodeStateVector(doc)) {
		t.Fatalf("re-sync changed the document: text %q -> %q", got, textOf(t, doc))
	}
}

var _ = id.RoomID("")

// TestDendrite_HostileEventInRealRoom: the untrusted-input claim, against a
// real homeserver rather than the double. A room member posts an event of our
// type whose `content` is a string instead of an object - the server stores
// and serves it without complaint - and a legitimate update published
// afterwards must still arrive.
//
// The unit test for this uses a hand-written response; this one proves the
// server really does hand such an event back, so the tolerance is not
// defending against an imaginary shape.
func TestDendrite_HostileEventInRealRoom(t *testing.T) {
	requireHomeserver(t)
	ctx := context.Background()

	author := register(t, uniq("hostile"))
	created, err := author.CreateRoom(ctx, &mautrix.ReqCreateRoom{Preset: "public_chat", InitialState: sharedHistory()})
	if err != nil {
		t.Fatal(err)
	}
	tr, _ := ymatrix.New(author, created.RoomID)

	// A raw send, because the client library would insist on an object here.
	txnID := fmt.Sprintf("hostile%d", time.Now().UnixNano())
	u := fmt.Sprintf("%s/_matrix/client/v3/rooms/%s/send/%s/%s",
		homeserverURL, url.PathEscape(string(created.RoomID)), ymatrix.EventType.Type, txnID)
	req, err := http.NewRequest(http.MethodPut, u, strings.NewReader(`"i am a string, not an object"`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+author.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Skipf("this Dendrite refuses a non-object event content (%d %s); the hostile shape is unreachable here, and the unit test covers the client side", resp.StatusCode, body)
	}

	d := ygo.NewDocWithOptions(ygo.Options{ClientID: 909})
	txt := ygo.NewText(d, "t")
	txn := d.WriteTxn()
	if err := txt.Insert(txn, 0, "survivor"); err != nil {
		t.Fatal(err)
	}
	txn.Commit()
	if _, err := tr.PublishDoc(ctx, d); err != nil {
		t.Fatalf("publish after the hostile event: %v", err)
	}

	_, got := syncFresh(t, tr, len("survivor"))
	if got != "survivor" {
		t.Fatalf("read %q back from a room containing one hostile event; a single bad publisher must not deny the room", got)
	}
}
