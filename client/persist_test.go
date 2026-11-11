package client_test

import (
	"context"
	"sync"
	"testing"

	"github.com/Deln0r/ygo/client"
	"github.com/Deln0r/ygo/internal/doc"
	"github.com/Deln0r/ygo/internal/types"
	"github.com/Deln0r/ygo/persist/sqlite"
)

// deadURL returns a ws:// URL nothing answers (a bound-then-closed
// listener), for exercising the offline path.
func deadURL(t *testing.T) string {
	t.Helper()
	ln := mustListen(t)
	url := "ws://" + ln.Addr().String()
	_ = ln.Close()
	return url
}

func openStore(t *testing.T, path string) *sqlite.Store {
	t.Helper()
	s, err := sqlite.Open(path)
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	return s
}

// connectClient builds + connects a client with a local store and a doc
// of the given clientID.
func connectClient(t *testing.T, url, path string, clientID uint64, store *sqlite.Store) *client.Client {
	t.Helper()
	c, err := client.New(client.Options{
		URL:        url,
		DocName:    "room",
		Doc:        doc.NewDocWithOptions(doc.Options{ClientID: clientID}),
		LocalStore: store,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	return c
}

// TestClient_OfflinePersistenceSurvivesRestart: an edit made while
// connected is persisted locally; a fresh client on the same store,
// pointed at a dead server, loads it offline with no network.
func TestClient_OfflinePersistenceSurvivesRestart(t *testing.T) {
	path := t.TempDir() + "/local.db"
	url := startServer(t)

	// Session 1: connect, edit, close (Close flushes the final state).
	st1 := openStore(t, path)
	a := connectClient(t, url, path, 1, st1)
	waitFor(t, "a synced", a.Synced)
	am := types.NewMap(a.Doc().Branch("settings"))
	txn := a.Doc().WriteTxn()
	am.Set(txn, "k", "v")
	txn.Commit()
	_ = a.Close() // final persist + flush lands here
	_ = st1.Close()

	// Session 2: restart on the same store, OFFLINE (dead server). The
	// document must be present from the local load alone.
	st2 := openStore(t, path)
	t.Cleanup(func() { _ = st2.Close() })
	b := connectClient(t, deadURL(t), path, 2, st2)
	t.Cleanup(func() { _ = b.Close() })

	// loadLocal runs synchronously inside Connect, so the edit is already
	// there; no network involved.
	if got := getKey(b, "k"); got != "v" {
		t.Errorf("offline-loaded key = %v, want v", got)
	}
}

// TestClient_OfflineEditsFlowUpOnReconnect: an edit made while the
// client cannot reach the server is persisted, then carried up to the
// server when a later session connects, and reaches a second client.
func TestClient_OfflineEditsFlowUpOnReconnect(t *testing.T) {
	path := t.TempDir() + "/local.db"

	// Session 1: never reaches a server (dead URL); the edit only lands
	// in the local store.
	st1 := openStore(t, path)
	a := connectClient(t, deadURL(t), path, 7, st1)
	am := types.NewMap(a.Doc().Branch("settings"))
	txn := a.Doc().WriteTxn()
	am.Set(txn, "offline", "yes")
	txn.Commit()
	_ = a.Close()
	_ = st1.Close()

	// Session 2: same store, now a live server. loadLocal restores the
	// offline edit, the handshake state vector covers it, and the server
	// pulls it up.
	url := startServer(t)
	st2 := openStore(t, path)
	t.Cleanup(func() { _ = st2.Close() })
	a2 := connectClient(t, url, path, 7, st2)
	t.Cleanup(func() { _ = a2.Close() })
	waitFor(t, "a2 synced", a2.Synced)
	// Loading session 1's edit, made under ID 7, gave the document a new
	// ID (as yjs does when it loads its own history); the edit still goes
	// up, since the handshake works on state vectors.
	if a2.Doc().ClientID() == 7 {
		t.Fatal("the reloaded document kept client ID 7, which its stored edit used")
	}

	// A second, store-less client on the same server sees the edit that
	// was originally made offline.
	other := dial(t, url, "room", 8)
	waitFor(t, "offline edit reached peer", func() bool {
		return getKey(other, "offline") == "yes"
	})
}

// TestClient_ConcurrentClose confirms Close is safe under concurrent
// and repeated calls (the teardown runs once). Run under -race; the
// pre-fix code raced on the observer-unsubscribe field.
func TestClient_ConcurrentClose(t *testing.T) {
	path := t.TempDir() + "/local.db"
	url := startServer(t)
	st := openStore(t, path)
	t.Cleanup(func() { _ = st.Close() })

	c := connectClient(t, url, path, 30, st)
	waitFor(t, "synced", c.Synced)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = c.Close()
		}()
	}
	wg.Wait()
	// A further call is still a no-op.
	if err := c.Close(); err != nil {
		t.Errorf("post-close Close: %v", err)
	}
}

// TestClient_RemoteUpdatesPersistedLocally: updates received from the
// server are written to the local store, so a fresh offline client
// reads server state with no network.
func TestClient_RemoteUpdatesPersistedLocally(t *testing.T) {
	path := t.TempDir() + "/local.db"
	url := startServer(t)

	// A store-less peer makes an edit on the server.
	peer := dial(t, url, "room", 20)
	pm := mapOf(peer)
	ptxn := peer.Doc().WriteTxn()
	pm.Set(ptxn, "server", "side")
	ptxn.Commit()

	// A store-backed client connects, receives the edit, and persists it.
	st1 := openStore(t, path)
	c := connectClient(t, url, path, 21, st1)
	waitFor(t, "client synced", c.Synced)
	waitFor(t, "client got remote edit", func() bool { return getKey(c, "server") == "side" })
	_ = c.Close()
	_ = st1.Close()

	// Restart offline: the server-side edit is in the local store.
	st2 := openStore(t, path)
	t.Cleanup(func() { _ = st2.Close() })
	c2 := connectClient(t, deadURL(t), path, 22, st2)
	t.Cleanup(func() { _ = c2.Close() })
	if got := getKey(c2, "server"); got != "side" {
		t.Errorf("offline-loaded remote key = %v, want side", got)
	}
}
