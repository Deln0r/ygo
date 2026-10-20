package client_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Deln0r/ygo/client"
	"github.com/Deln0r/ygo/internal/doc"
	"github.com/Deln0r/ygo/internal/encoding"
	"github.com/Deln0r/ygo/internal/types"
	"github.com/Deln0r/ygo/server"
)

func startServer(t *testing.T) string {
	t.Helper()
	srv := server.New(server.Options{OriginPatterns: []string{"*"}})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		ts.Close()
		_ = srv.Close(context.Background())
	})
	return "ws://" + strings.TrimPrefix(ts.URL, "http://")
}

func dial(t *testing.T, url, docName string, clientID uint64) *client.Client {
	t.Helper()
	c, err := client.New(client.Options{
		URL:     url,
		DocName: docName,
		Doc:     doc.NewDocWithOptions(doc.Options{ClientID: clientID}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

func mapOf(c *client.Client) *types.Map {
	return types.NewMap(c.Doc().Branch("settings"))
}

// getKey reads one map key under a read transaction: with a live
// client applying remote updates concurrently, unsynchronized reads
// race (the wrappers' lock-free read contract requires the caller to
// hold a transaction).
func getKey(c *client.Client, key string) any {
	m := mapOf(c)
	rtxn := c.Doc().ReadTxn()
	defer rtxn.Close()
	return m.Get(key)
}

// TestClient_TwoClients_Converge: a local edit on A reaches B through
// a real server, with no manual frame plumbing.
func TestClient_TwoClients_Converge(t *testing.T) {
	url := startServer(t)
	a := dial(t, url, "room", 41)
	b := dial(t, url, "room", 42)
	waitFor(t, "a synced", a.Synced)
	waitFor(t, "b synced", b.Synced)

	am := mapOf(a)
	txn := a.Doc().WriteTxn()
	am.Set(txn, "theme", "dark")
	txn.Commit()

	waitFor(t, "b sees theme", func() bool {
		return getKey(b, "theme") == "dark"
	})

	// And the reverse direction.
	bm := mapOf(b)
	txn = b.Doc().WriteTxn()
	bm.Set(txn, "lang", "go")
	txn.Commit()
	waitFor(t, "a sees lang", func() bool {
		return getKey(a, "lang") == "go"
	})
}

// TestClient_LateJoiner_GetsHistory: edits made before B ever
// connected arrive via the handshake.
func TestClient_LateJoiner_GetsHistory(t *testing.T) {
	url := startServer(t)
	a := dial(t, url, "room", 43)
	waitFor(t, "a synced", a.Synced)

	am := mapOf(a)
	txn := a.Doc().WriteTxn()
	am.Set(txn, "k", "early")
	txn.Commit()
	waitFor(t, "server has the edit", func() bool {
		// A round-trip through a second observer is the only external
		// signal; just give the update a beat to land.
		return true
	})

	b := dial(t, url, "room", 44)
	waitFor(t, "b sees pre-connect edit", func() bool {
		return getKey(b, "k") == "early"
	})
}

// TestClient_OfflineEditsFlowOnConnect: edits made BEFORE Connect are
// pushed up by the handshake diff.
func TestClient_OfflineEditsFlowOnConnect(t *testing.T) {
	url := startServer(t)

	offline, err := client.New(client.Options{
		URL: url, DocName: "room",
		Doc: doc.NewDocWithOptions(doc.Options{ClientID: 45}),
	})
	if err != nil {
		t.Fatal(err)
	}
	om := types.NewMap(offline.Doc().Branch("settings"))
	txn := offline.Doc().WriteTxn()
	om.Set(txn, "made", "offline")
	txn.Commit()

	if err := offline.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = offline.Close() })

	b := dial(t, url, "room", 46)
	waitFor(t, "offline edit reached b", func() bool {
		return getKey(b, "made") == "offline"
	})
}

// TestClient_AwarenessPropagates: A's awareness state shows up in B's
// awareness map.
func TestClient_AwarenessPropagates(t *testing.T) {
	url := startServer(t)
	a := dial(t, url, "room", 47)
	b := dial(t, url, "room", 48)
	waitFor(t, "b synced", b.Synced)

	a.SetAwarenessState([]byte(`{"name":"alice"}`))

	waitFor(t, "b sees alice", func() bool {
		states := b.Awareness().States()
		return string(states[47]) == `{"name":"alice"}`
	})
}

// TestClient_SyncedCallback fires true after the handshake.
func TestClient_SyncedCallback(t *testing.T) {
	url := startServer(t)
	synced := make(chan bool, 4)
	c, err := client.New(client.Options{
		URL: url, DocName: "room",
		OnSynced: func(v bool) { synced <- v },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	select {
	case v := <-synced:
		if !v {
			t.Error("first OnSynced = false, want true")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OnSynced never fired")
	}
}

// After a remote update, a local edit sends only this client's own change.
// The remote content came from the server, which has it; it used to go back
// with the next local edit, so in a room with several writers every message
// also carried the others' changes since the sender's last send, and large
// ones went past the server's ReadLimit.
func TestClient_LocalEditCarriesOnlyOwnChanges(t *testing.T) {
	var mu sync.Mutex
	var updates [][]byte // every update a client sent, as the server applied it
	srv := server.New(server.Options{
		OriginPatterns: []string{"*"},
		OnChange: func(_ string, update []byte) {
			mu.Lock()
			updates = append(updates, append([]byte(nil), update...))
			mu.Unlock()
		},
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		ts.Close()
		_ = srv.Close(context.Background())
	})
	// sentBy returns the first update the server applied that carries an
	// item of client cid, or nil. OnChange runs after the update is queued
	// to the room, so a peer can see an edit before its callback has run.
	sentBy := func(cid uint64) []byte {
		mu.Lock()
		defer mu.Unlock()
		for _, u := range updates {
			if dec, _, err := encoding.DecodeUpdate(u); err == nil && len(dec.Blocks[cid]) > 0 {
				return u
			}
		}
		return nil
	}
	url := "ws://" + strings.TrimPrefix(ts.URL, "http://")
	a := dial(t, url, "room", 51)
	b := dial(t, url, "room", 52)
	waitFor(t, "a synced", a.Synced)
	waitFor(t, "b synced", b.Synced)

	am, bm := mapOf(a), mapOf(b) // before any transaction: Branch takes the doc lock
	big := strings.Repeat("x", 20000)
	txn := a.Doc().WriteTxn()
	am.Set(txn, "big", big)
	txn.Commit()
	waitFor(t, "b sees big", func() bool { return getKey(b, "big") == big })

	txn = b.Doc().WriteTxn()
	bm.Set(txn, "small", "y")
	txn.Commit()
	waitFor(t, "the server applies B's edit", func() bool { return sentBy(52) != nil })
	if n := len(sentBy(52)); n > 1000 {
		t.Fatalf("B's one small edit went out as a %d-byte update: it carried A's change back", n)
	}
}
