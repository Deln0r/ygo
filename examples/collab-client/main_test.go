package main

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Deln0r/ygo"
	"github.com/Deln0r/ygo/client"
	"github.com/Deln0r/ygo/server"
)

// TestExample_ClientSyncsWithServer drives the example client against an
// in-process ygo server: a writer client appends an entry and a reader
// client on the same document converges to it. This proves the example
// actually connects, edits, and syncs, not merely that it compiles.
func TestExample_ClientSyncsWithServer(t *testing.T) {
	srv := server.New(server.Options{OriginPatterns: []string{"*"}})
	t.Cleanup(func() { _ = srv.Close(context.Background()) })
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Writer connects, syncs, and appends "hello".
	w, wItems, err := newClient(wsURL, "room")
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	waitSynced(w, 5*time.Second)
	txn := w.Doc().WriteTxn()
	wItems.Push(txn, "hello")
	txn.Commit()

	// Reader on the same document must converge to the writer's entry.
	r, rItems, err := newClient(wsURL, "room")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	deadline := time.Now().Add(5 * time.Second)
	for readLen(r, rItems) < 1 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := readLen(r, rItems); got != 1 {
		t.Fatalf("reader items = %d, want 1 (did not converge)", got)
	}
}

// readLen reads the array length under a read transaction. A bare Len()
// races with the client's apply goroutine mutating the same doc; the read
// txn serializes against the write, which is the pattern adopters must
// use to inspect a live client document off its own goroutines.
func readLen(c *client.Client, a *ygo.Array) int {
	rtxn := c.Doc().ReadTxn()
	defer rtxn.Close()
	return int(a.Len())
}
