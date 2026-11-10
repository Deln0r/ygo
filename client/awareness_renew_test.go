package client_test

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Deln0r/ygo/client"
	"github.com/Deln0r/ygo/internal/doc"
	"github.com/Deln0r/ygo/server"
)

// serveOn runs srv's handler on a tracking listener and returns its URL.
func serveOn(t *testing.T, srv *server.Server) (string, *trackingListener) {
	t.Helper()
	ln := mustListen(t)
	hs := &http.Server{Handler: srv.Handler()}
	go hs.Serve(ln)
	t.Cleanup(func() { _ = hs.Close() })
	return "ws://" + ln.Addr().String(), ln
}

func dialOpts(t *testing.T, opts client.Options, clientID uint64) *client.Client {
	t.Helper()
	opts.Doc = doc.NewDocWithOptions(doc.Options{ClientID: clientID})
	c, err := client.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// An idle client's presence outlives the server's awareness timeout: the
// client renews its state, as y-protocols does every 15 seconds. Before, it
// sent its state once, so the server swept it after the timeout and every
// peer saw it leave while it was still connected.
func TestClient_AwarenessRenewedPastServerTimeout(t *testing.T) {
	srv := server.New(server.Options{OriginPatterns: []string{"*"}, AwarenessTimeout: 2 * time.Second})
	t.Cleanup(func() { _ = srv.Close(context.Background()) })
	url, _ := serveOn(t, srv)

	a := dialOpts(t, client.Options{URL: url, DocName: "room", AwarenessTimeout: time.Second}, 61)
	b := dialOpts(t, client.Options{URL: url, DocName: "room"}, 62)
	waitFor(t, "a synced", a.Synced)
	waitFor(t, "b synced", b.Synced)
	a.SetAwarenessState([]byte(`{"name":"a"}`))
	waitFor(t, "b sees a", func() bool { return b.Awareness().States()[61] != nil })

	// Three times the server's timeout: without renewal the sweep removes
	// a within its timeout plus one sweep tick.
	for end := time.Now().Add(6 * time.Second); time.Now().Before(end); {
		if b.Awareness().States()[61] == nil {
			t.Fatal("b lost a's presence while a was connected")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A client that reconnects is visible again at once. The server keeps the
// clock of a disconnected client's removed state, so a state sent again at
// that clock was ignored, and the client stayed invisible until its next
// change.
func TestClient_AwarenessVisibleAfterReconnect(t *testing.T) {
	srv := server.New(server.Options{OriginPatterns: []string{"*"}})
	t.Cleanup(func() { _ = srv.Close(context.Background()) })
	urlA, lnA := serveOn(t, srv)
	urlB, _ := serveOn(t, srv)

	a := dialOpts(t, client.Options{URL: urlA, DocName: "room"}, 63)
	b := dialOpts(t, client.Options{URL: urlB, DocName: "room"}, 64)
	waitFor(t, "a synced", a.Synced)
	waitFor(t, "b synced", b.Synced)
	a.SetAwarenessState([]byte(`{"name":"a"}`))
	waitFor(t, "b sees a", func() bool { return b.Awareness().States()[63] != nil })

	lnA.dropAll()
	waitFor(t, "b sees a leave", func() bool { return b.Awareness().States()[63] == nil })
	waitFor(t, "a resynced", a.Synced)
	waitFor(t, "b sees a again", func() bool { return string(b.Awareness().States()[63]) == `{"name":"a"}` })
}

// reconnectedClient dials through its own listener and returns a channel
// that receives once per completed handshake after the first.
func reconnectedClient(t *testing.T, srv *server.Server, clientID uint64) (*client.Client, *trackingListener, chan struct{}) {
	t.Helper()
	url, ln := serveOn(t, srv)
	resynced := make(chan struct{}, 4)
	var handshakes atomic.Int32
	c := dialOpts(t, client.Options{URL: url, DocName: "room", OnSynced: func(v bool) {
		if v && handshakes.Add(1) > 1 {
			resynced <- struct{}{}
		}
	}}, clientID)
	waitFor(t, "synced", c.Synced)
	return c, ln, resynced
}

func waitChan(t *testing.T, what string, ch chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timeout waiting for %s", what)
	}
}

// A peer that left while this client was offline is gone once the client
// reconnects. The server answers the reconnect with the live states only,
// so a peer this client kept would stay forever; y-websocket drops every
// remote state when its connection closes, and so does the client now.
func TestClient_AwarenessForgetsPeersAcrossReconnect(t *testing.T) {
	srv := server.New(server.Options{OriginPatterns: []string{"*"}})
	t.Cleanup(func() { _ = srv.Close(context.Background()) })
	urlA, _ := serveOn(t, srv)
	a := dialOpts(t, client.Options{URL: urlA, DocName: "room"}, 65)
	b, lnB, resynced := reconnectedClient(t, srv, 66)
	waitFor(t, "a synced", a.Synced)
	a.SetAwarenessState([]byte(`{"name":"a"}`))
	waitFor(t, "b sees a", func() bool { return b.Awareness().States()[65] != nil })

	lnB.dropAll()
	_ = a.Close()
	waitChan(t, "b to reconnect", resynced)
	time.Sleep(200 * time.Millisecond)
	if b.Awareness().States()[65] != nil {
		t.Error("b still shows a, which left while b was offline")
	}
}

// A peer still online is visible again as soon as this client reconnects,
// though it sent nothing new: its state comes back at the clock this
// client saw before, which a kept tombstone would refuse until the peer's
// next renewal, up to 15 seconds later.
func TestClient_AwarenessKeepsOnlinePeersAcrossReconnect(t *testing.T) {
	srv := server.New(server.Options{OriginPatterns: []string{"*"}})
	t.Cleanup(func() { _ = srv.Close(context.Background()) })
	urlA, _ := serveOn(t, srv)
	a := dialOpts(t, client.Options{URL: urlA, DocName: "room"}, 67)
	b, lnB, resynced := reconnectedClient(t, srv, 68)
	waitFor(t, "a synced", a.Synced)
	a.SetAwarenessState([]byte(`{"name":"a"}`))
	waitFor(t, "b sees a", func() bool { return b.Awareness().States()[67] != nil })

	lnB.dropAll()
	waitChan(t, "b to reconnect", resynced)
	waitFor(t, "b sees a after reconnecting", func() bool { return string(b.Awareness().States()[67]) == `{"name":"a"}` })
}
