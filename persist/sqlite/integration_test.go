package sqlite_test

import (
	"context"
	"net"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Deln0r/ygo"
	"github.com/Deln0r/ygo/client"
	"github.com/Deln0r/ygo/persist/sqlite"
	"github.com/Deln0r/ygo/server"
)

// TestRestart_ClientAndServerOnSQLite: a client and a server that keep
// their documents in SQLite files come back after a restart with what was
// written, the client offline from its own file and the server from its.
// The core module's tests run the same restarts on its in-memory store.
func TestRestart_ClientAndServerOnSQLite(t *testing.T) {
	dir := t.TempDir()
	open := func(name string) *sqlite.Store {
		t.Helper()
		s, err := sqlite.Open(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	serve := func(st *sqlite.Store) (string, func()) {
		srv := server.New(server.Options{Store: st, OriginPatterns: []string{"*"}})
		h := httptest.NewServer(srv.Handler())
		return "ws://" + strings.TrimPrefix(h.URL, "http://"), func() {
			h.Close()
			_ = srv.Close(context.Background())
		}
	}
	connect := func(url string, id uint64, st *sqlite.Store) *client.Client {
		t.Helper()
		c, err := client.New(client.Options{URL: url, DocName: "room", Doc: ygo.NewDocWithOptions(ygo.Options{ClientID: id}), LocalStore: st})
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Connect(context.Background()); err != nil {
			t.Fatal(err)
		}
		return c
	}
	// key reads under a read transaction: the client's goroutine applies
	// updates to the same document.
	key := func(c *client.Client) any {
		m := ygo.NewMap(c.Doc(), "settings")
		r := c.Doc().ReadTxn()
		defer r.Close()
		return m.Get("k")
	}
	wait := func(what string, cond func() bool) {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); !cond(); time.Sleep(10 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
		}
	}

	// Session 1: a client edits through a server; everything closes.
	serverStore := open("server.db")
	url, stop := serve(serverStore)
	localStore := open("local.db")
	a := connect(url, 1, localStore)
	wait("a synced", a.Synced)
	m := ygo.NewMap(a.Doc(), "settings")
	w := a.Doc().WriteTxn()
	m.Set(w, "k", "v")
	w.Commit()
	b := connect(url, 2, open("other.db"))
	wait("the edit at the server", func() bool { return key(b) == "v" })
	_ = b.Close()
	_ = a.Close()
	_ = localStore.Close()
	stop()
	_ = serverStore.Close()

	// The client restarts offline on its file.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := "ws://" + ln.Addr().String()
	_ = ln.Close()
	localStore = open("local.db")
	t.Cleanup(func() { _ = localStore.Close() })
	offline := connect(dead, 3, localStore)
	t.Cleanup(func() { _ = offline.Close() })
	if got := key(offline); got != "v" {
		t.Fatalf("the client restarted offline reads %v, want v", got)
	}

	// The server restarts on its file and serves a new client.
	serverStore = open("server.db")
	t.Cleanup(func() { _ = serverStore.Close() })
	url, stop = serve(serverStore)
	t.Cleanup(stop)
	fresh := connect(url, 4, open("fresh.db"))
	t.Cleanup(func() { _ = fresh.Close() })
	wait("the restarted server's state", func() bool { return key(fresh) == "v" })
}
