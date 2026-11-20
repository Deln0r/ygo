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

// TestRestart_ClientAndServerOnSQLite: clients and a server that keep
// their documents in SQLite files come back after a restart with what was
// written: an edit a client made, an edit a client received, an edit made
// offline (carried to the server on reconnect), and the server's state.
// The core module's tests run the same restarts on its in-memory store.
func TestRestart_ClientAndServerOnSQLite(t *testing.T) {
	dir := t.TempDir()
	// open registers every store's Close (idempotent), so no file is left
	// open when the temporary directory goes (Windows refuses to delete an
	// open SQLite file; Client.Close does not close its LocalStore).
	open := func(name string) *sqlite.Store {
		t.Helper()
		s, err := sqlite.Open(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
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
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	// key reads under a read transaction: the client's goroutine applies
	// updates to the same document.
	key := func(c *client.Client, k string) any {
		m := ygo.NewMap(c.Doc(), "settings")
		r := c.Doc().ReadTxn()
		defer r.Close()
		return m.Get(k)
	}
	set := func(c *client.Client, k, v string) {
		m := ygo.NewMap(c.Doc(), "settings")
		w := c.Doc().WriteTxn()
		m.Set(w, k, v)
		w.Commit()
	}
	wait := func(what string, cond func() bool) {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); !cond(); time.Sleep(10 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
		}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := "ws://" + ln.Addr().String()
	_ = ln.Close()

	// Session 1: a client edits through a server, another receives it; an
	// offline client edits with no server at all; everything closes.
	serverStore := open("server.db")
	url, stop := serve(serverStore)
	writerStore, receiverStore, offlineStore := open("writer.db"), open("receiver.db"), open("offline.db")
	a := connect(url, 1, writerStore)
	wait("a synced", a.Synced)
	set(a, "k", "v")
	b := connect(url, 2, receiverStore)
	wait("the edit at the receiver", func() bool { return key(b, "k") == "v" })
	o := connect(dead, 3, offlineStore)
	set(o, "offline", "w")
	for _, c := range []*client.Client{a, b, o} {
		_ = c.Close()
	}
	for _, s := range []*sqlite.Store{writerStore, receiverStore, offlineStore} {
		_ = s.Close()
	}
	stop()
	_ = serverStore.Close()

	// The writer and the receiver restart offline on their files.
	for name, id := range map[string]uint64{"writer.db": 4, "receiver.db": 5} {
		c := connect(dead, id, open(name))
		if got := key(c, "k"); got != "v" {
			t.Fatalf("%s restarted offline reads %v, want v", name, got)
		}
	}

	// The server restarts on its file; the offline client reconnects and
	// its edit reaches the server, which serves both edits to a new client.
	url, stop = serve(open("server.db"))
	t.Cleanup(stop)
	back := connect(url, 6, open("offline.db"))
	wait("the offline client synced", back.Synced)
	fresh := connect(url, 7, open("fresh.db"))
	wait("the restarted server's state and the offline edit", func() bool {
		return key(fresh, "k") == "v" && key(fresh, "offline") == "w"
	})
}
