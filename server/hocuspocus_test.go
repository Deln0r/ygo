package server_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Deln0r/ygo/internal/awareness"
	"github.com/Deln0r/ygo/internal/doc"
	"github.com/Deln0r/ygo/internal/encoding"
	syncpkg "github.com/Deln0r/ygo/internal/sync"
	"github.com/Deln0r/ygo/internal/types"
	"github.com/Deln0r/ygo/server"
	"github.com/Deln0r/ygo/server/backplane"
)

type hpSocket struct {
	ws  *websocket.Conn
	ctx context.Context
}

func hpDial(t *testing.T, url string) *hpSocket {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	c, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.CloseNow(); cancel() })
	return &hpSocket{c, ctx}
}

func (c *hpSocket) send(t *testing.T, key string, frame []byte) {
	t.Helper()
	if err := c.ws.Write(c.ctx, websocket.MessageBinary, append(syncpkg.EncodeRoute(key), frame...)); err != nil {
		t.Fatal(err)
	}
}

func (c *hpSocket) read(t *testing.T) (string, *syncpkg.Frame) {
	t.Helper()
	_, raw, err := c.ws.Read(c.ctx)
	if err != nil {
		t.Fatal(err)
	}
	key, body, err := syncpkg.DecodeRoute(raw)
	if err != nil {
		t.Fatal(err)
	}
	f, _, err := syncpkg.DecodeEnvelope(body)
	if err != nil {
		t.Fatal(err)
	}
	return key, f
}

func (c *hpSocket) want(t *testing.T, key string, typ syncpkg.MessageType) *syncpkg.Frame {
	t.Helper()
	got, f := c.read(t)
	if got != key || f.Type != typ {
		t.Fatalf("key=%q frame=%+v, want %q type %d", got, f, key, typ)
	}
	return f
}

func (c *hpSocket) auth(t *testing.T, key, token, scope string) {
	t.Helper()
	c.send(t, key, syncpkg.EncodeAuthToken(token))
	f := c.want(t, key, syncpkg.MessageAuth)
	if f.AuthSub != syncpkg.AuthAuthenticated || string(f.Payload) != scope {
		t.Fatalf("auth: %+v", f)
	}
}

func hpUpdate(t *testing.T, value string) []byte {
	t.Helper()
	d := doc.NewDoc()
	arr := types.NewArray(d.Branch("items"))
	txn := d.WriteTxn()
	arr.Push(txn, value)
	txn.Commit()
	return encoding.EncodeStateAsUpdate(d)
}

func hpValues(t *testing.T, update []byte) []any {
	t.Helper()
	d := doc.NewDoc()
	if err := encoding.ApplyUpdate(d, update); err != nil {
		t.Fatal(err)
	}
	return types.NewArray(d.Branch("items")).ToSlice()
}

func hpSync(t *testing.T, c *hpSocket, key string) []any {
	t.Helper()
	c.send(t, key, syncpkg.EncodeSyncStep1([]byte{0}))
	if f := c.want(t, key, syncpkg.MessageSync); f.SyncSub != syncpkg.SyncStep1 {
		t.Fatalf("first sync: %+v", f)
	}
	f := c.want(t, key, syncpkg.MessageSync)
	if f.SyncSub != syncpkg.SyncStep2 {
		t.Fatalf("second sync: %+v", f)
	}
	return hpValues(t, f.Payload)
}

func hpAwaitStats(t *testing.T, s *server.Server, docs, conns int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		st := s.Stats()
		if st.Documents == docs && st.Connections == conns {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("stats: %+v, want docs=%d keys=%d", s.Stats(), docs, conns)
}

func TestHocuspocusAuthQueueIsolationAndRetry(t *testing.T) {
	var loads atomic.Int32
	var mu sync.Mutex
	var tokens []string
	url, s := startTestServer(t, server.Options{
		OnAuthenticate: func(name, token string) error {
			mu.Lock()
			tokens = append(tokens, name+":"+token)
			mu.Unlock()
			if token != name {
				return errors.New("wrong document")
			}
			return nil
		},
		OnLoadDocument: func(string) ([]byte, error) { loads.Add(1); return nil, nil },
	})
	c := hpDial(t, url)
	// B stays pending while A is admitted. Nothing loads before its own token.
	c.send(t, "b", syncpkg.EncodeSyncUpdate(hpUpdate(t, "discard-on-deny")))
	c.send(t, "a", syncpkg.EncodeSyncStep1([]byte{0}))
	c.send(t, "a", syncpkg.EncodeSyncUpdate(hpUpdate(t, "queued-a")))
	c.auth(t, "a", "a", syncpkg.ScopeReadWrite)
	if f := c.want(t, "a", syncpkg.MessageSync); f.SyncSub != syncpkg.SyncStep1 {
		t.Fatal(f)
	}
	if f := c.want(t, "a", syncpkg.MessageSync); len(hpValues(t, f.Payload)) != 0 {
		t.Fatal("queue reordered")
	}
	c.want(t, "a", syncpkg.MessageSyncStatus)
	if f := c.want(t, "a", syncpkg.MessageSync); fmt.Sprint(hpValues(t, f.Payload)) != "[queued-a]" {
		t.Fatal(f)
	}
	if loads.Load() != 1 {
		t.Fatal("unauthenticated document loaded")
	}
	hpAwaitStats(t, s, 1, 1)
	c.send(t, "b", syncpkg.EncodeAuthToken("a"))
	if f := c.want(t, "b", syncpkg.MessageAuth); f.AuthSub != syncpkg.AuthPermissionDenied || string(f.Payload) != "wrong document" {
		t.Fatal(f)
	}
	if loads.Load() != 1 {
		t.Fatal("denied document loaded")
	}
	c.auth(t, "b", "b", syncpkg.ScopeReadWrite)
	if got := hpSync(t, c, "b"); len(got) != 0 {
		t.Fatalf("denied queue survived: %v", got)
	}
	if got := hpSync(t, c, "a"); fmt.Sprint(got) != "[queued-a]" {
		t.Fatal(got)
	}
	hpAwaitStats(t, s, 2, 2)
	mu.Lock()
	defer mu.Unlock()
	if fmt.Sprint(tokens) != "[a:a b:a b:b]" {
		t.Fatal(tokens)
	}
}

func TestHocuspocusSessionKeysCloseAndMixedProtocols(t *testing.T) {
	disconnected := make(chan string, 8)
	url, s := startTestServer(t, server.Options{OnDisconnect: func(_, name string) { disconnected <- name }})
	c := hpDial(t, url)
	for _, k := range []string{"room\x00one", "room\x00two", "other"} {
		c.auth(t, k, "", syncpkg.ScopeReadWrite)
	}
	y := dialClient(t, url, "room", 912)
	defer y.close()
	y.read(t)
	hpAwaitStats(t, s, 2, 4)
	c.send(t, "room\x00one", syncpkg.EncodeSyncUpdate(hpUpdate(t, "shared")))
	c.want(t, "room\x00one", syncpkg.MessageSyncStatus)
	seen := map[string]bool{}
	for range 2 {
		k, f := c.read(t)
		seen[k] = f.Type == syncpkg.MessageSync && fmt.Sprint(hpValues(t, f.Payload)) == "[shared]"
	}
	if !seen["room\x00one"] || !seen["room\x00two"] {
		t.Fatal(seen)
	}
	if f := y.read(t); f.Type != syncpkg.MessageSync || fmt.Sprint(hpValues(t, f.Payload)) != "[shared]" {
		t.Fatal(f)
	}
	c.send(t, "room\x00one", []byte{7})
	if f := c.want(t, "room\x00one", syncpkg.MessageClose); string(f.Payload) != "provider_initiated" {
		t.Fatal(f)
	}
	if name := <-disconnected; name != "room" {
		t.Fatal(name)
	}
	hpAwaitStats(t, s, 2, 3)
	if got := hpSync(t, c, "other"); len(got) != 0 {
		t.Fatal(got)
	}
	if got := hpSync(t, c, "room\x00two"); fmt.Sprint(got) != "[shared]" {
		t.Fatal(got)
	}
	// The remaining key and the unprefixed peer still share updates.
	pushEdit(t, y, "from-y-websocket")
	if f := c.want(t, "room\x00two", syncpkg.MessageSync); !containsAll(hpValues(t, f.Payload), "from-y-websocket") {
		t.Fatal(f)
	}
}

func TestHocuspocusReadOnlyRequestAndStateless(t *testing.T) {
	var legacy atomic.Int32
	url, s := startTestServer(t, server.Options{
		OnAuthenticate: func(_, _ string) error { legacy.Add(1); return errors.New("must not be called") },
		OnAuthenticateRequest: func(r *http.Request, name, token string) (bool, error) {
			if r.URL.Query().Get("cookie") != "present" || name != "room" {
				return false, errors.New("missing request context")
			}
			return token == "ro", nil
		},
	})
	c := hpDial(t, url+"?cookie=present")
	c.auth(t, "room\x00ro", "ro", syncpkg.ScopeReadOnly)
	c.auth(t, "room\x00rw", "rw", syncpkg.ScopeReadWrite)
	c.send(t, "room\x00ro", syncpkg.EncodeSyncUpdate(hpUpdate(t, "forbidden")))
	if f := c.want(t, "room\x00ro", syncpkg.MessageSyncStatus); f.Payload[0] != 0 {
		t.Fatal("readonly accepted edit")
	}
	if got := hpSync(t, c, "room\x00rw"); len(got) != 0 {
		t.Fatalf("readonly edit reached peer: %v", got)
	}
	c.send(t, "room\x00ro", syncpkg.EncodeSyncStep2([]byte{0, 0}))
	if f := c.want(t, "room\x00ro", syncpkg.MessageSyncStatus); f.Payload[0] != 1 {
		t.Fatal("empty readonly sync should ack")
	}
	// A later token must not upgrade the existing key's authority.
	c.auth(t, "room\x00ro", "rw", syncpkg.ScopeReadOnly)
	if legacy.Load() != 0 {
		t.Fatal("legacy hook called")
	}
	y := dialClient(t, url, "room", 913)
	defer y.close()
	y.read(t)
	s.SendStateless("room", "hello")
	seen := map[string]bool{}
	for range 2 {
		k, f := c.read(t)
		seen[k] = f.Type == syncpkg.MessageStateless && string(f.Payload) == "hello"
	}
	if !seen["room\x00ro"] || !seen["room\x00rw"] {
		t.Fatal(seen)
	}
	c.send(t, "room\x00rw", syncpkg.EncodeBroadcastStateless("broadcast"))
	for range 2 {
		_, f := c.read(t)
		if f.Type != syncpkg.MessageStateless || string(f.Payload) != "broadcast" {
			t.Fatal(f)
		}
	}
	y.write(t, syncpkg.EncodeSyncStep1([]byte{0}))
	if f := y.read(t); f.Type != syncpkg.MessageSync || f.SyncSub != syncpkg.SyncStep2 {
		t.Fatal("stateless leaked to y-websocket", f)
	}
}

func TestHocuspocusNoTokenAndPerKeyError(t *testing.T) {
	url, s := startTestServer(t, server.Options{})
	c := hpDial(t, url)
	if got := hpSync(t, c, "a"); len(got) != 0 {
		t.Fatal(got)
	}
	c.auth(t, "b", "", syncpkg.ScopeReadWrite)
	// A valid envelope with an invalid state vector is a per-key error.
	c.send(t, "a", syncpkg.EncodeSyncStep1([]byte{128}))
	if f := c.want(t, "a", syncpkg.MessageClose); string(f.Payload) != "Reset Connection" {
		t.Fatal(f)
	}
	hpAwaitStats(t, s, 1, 1)
	hpSync(t, c, "b")
	// Invalid update payloads get their sync status but do not end the key.
	c.send(t, "b", syncpkg.EncodeSyncUpdate([]byte{255}))
	c.want(t, "b", syncpkg.MessageSyncStatus)
	hpSync(t, c, "b")
}

func TestHocuspocusConnectRejectionAndDisconnect(t *testing.T) {
	var disconnects atomic.Int32
	url, s := startTestServer(t, server.Options{
		OnConnect: func(_, name string, _ *http.Request) error {
			if name == "no" {
				return errors.New("rejected")
			}
			return nil
		},
		OnDisconnect: func(_, _ string) { disconnects.Add(1) },
	})
	c := hpDial(t, url)
	c.send(t, "no", syncpkg.EncodeAuthToken(""))
	if f := c.want(t, "no", syncpkg.MessageAuth); f.AuthSub != syncpkg.AuthPermissionDenied {
		t.Fatal(f)
	}
	hpAwaitStats(t, s, 0, 0)
	if disconnects.Load() != 0 {
		t.Fatal("rejected OnConnect got OnDisconnect")
	}
	c.auth(t, "yes", "", syncpkg.ScopeReadWrite)
	c.auth(t, "second", "", syncpkg.ScopeReadWrite)
	_ = c.ws.Close(websocket.StatusNormalClosure, "done")
	hpAwaitStats(t, s, 0, 0)
	deadline := time.Now().Add(time.Second)
	for disconnects.Load() != 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if disconnects.Load() != 2 {
		t.Fatal(disconnects.Load())
	}
}

func TestHocuspocusPendingLimits(t *testing.T) {
	for _, scenario := range []string{"keys", "messages", "bytes"} {
		t.Run(scenario, func(t *testing.T) {
			url, s := startTestServer(t, server.Options{OnAuthenticate: func(_, _ string) error { return nil }, ReadLimit: 6 << 20})
			c := hpDial(t, url)
			switch scenario {
			case "keys":
				for i := 0; i < 101; i++ {
					c.send(t, fmt.Sprint(i), syncpkg.EncodeSyncStep1([]byte{0}))
				}
			case "messages":
				for i := 0; i < 1001; i++ {
					c.send(t, fmt.Sprint(i%2), syncpkg.EncodeSyncStep1([]byte{0}))
				}
			case "bytes":
				for i := 0; i < 2; i++ {
					c.send(t, fmt.Sprint(i), syncpkg.EncodeStateless(strings.Repeat("x", 3<<20)))
				}
			}
			_, _, err := c.ws.Read(c.ctx)
			if websocket.CloseStatus(err) != 4205 {
				t.Fatalf("close: %v", err)
			}
			hpAwaitStats(t, s, 0, 0)
		})
	}
}

func TestHocuspocusSocketAndDocumentLimits(t *testing.T) {
	url, s := startTestServer(t, server.Options{MaxConns: 1})
	c := hpDial(t, url)
	// No established key is needed to consume MaxConns.
	other := hpDial(t, url)
	_, _, err := other.ws.Read(other.ctx)
	if websocket.CloseStatus(err) != websocket.StatusTryAgainLater {
		t.Fatal(err)
	}
	c.auth(t, "a", "", syncpkg.ScopeReadWrite)
	c.auth(t, "b", "", syncpkg.ScopeReadWrite)
	hpAwaitStats(t, s, 2, 2)
	for _, opts := range []server.Options{{MaxDocs: 1}, {MaxConnsPerDoc: 1}, {OnLoadDocument: func(string) ([]byte, error) { return nil, errors.New("load failure") }}} {
		u, srv := startTestServer(t, opts)
		x := hpDial(t, u)
		if opts.OnLoadDocument == nil {
			x.auth(t, "a", "", syncpkg.ScopeReadWrite)
		}
		name := "b"
		if opts.MaxConnsPerDoc > 0 {
			name = "a\x00two"
		}
		x.send(t, name, syncpkg.EncodeAuthToken(""))
		_, _, err := x.ws.Read(x.ctx)
		if websocket.CloseStatus(err) != websocket.StatusTryAgainLater {
			t.Fatal(err)
		}
		hpAwaitStats(t, srv, 0, 0)
	}
}

func TestHocuspocusTimeouts(t *testing.T) {
	t.Run("idle", func(t *testing.T) {
		url, s := startTestServer(t, server.Options{IdleTimeout: 100 * time.Millisecond, AuthTimeout: time.Second})
		c := hpDial(t, url)
		c.auth(t, "a", "", syncpkg.ScopeReadWrite)
		_, _, err := c.ws.Read(c.ctx)
		if websocket.CloseStatus(err) != 4408 {
			t.Fatal(err)
		}
		hpAwaitStats(t, s, 0, 0)
	})
	t.Run("absolute auth", func(t *testing.T) {
		url, s := startTestServer(t, server.Options{IdleTimeout: time.Second, AuthTimeout: 100 * time.Millisecond, OnAuthenticate: func(_, _ string) error { return nil }})
		c := hpDial(t, url)
		stop := make(chan struct{})
		done := make(chan struct{})
		go func() {
			defer close(done)
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-stop:
					return
				case <-ticker.C:
					if c.ws.Write(c.ctx, websocket.MessageBinary, append(syncpkg.EncodeRoute("a"), syncpkg.EncodeSyncStep1([]byte{0})...)) != nil {
						return
					}
				}
			}
		}()
		_, _, err := c.ws.Read(c.ctx)
		close(stop)
		<-done
		if websocket.CloseStatus(err) != 4408 {
			t.Fatal(err)
		}
		hpAwaitStats(t, s, 0, 0)
	})
}

func TestHocuspocusMountsAndMisrouting(t *testing.T) {
	for _, path := range []string{"/", "/collab", "/collab/"} {
		t.Run(path, func(t *testing.T) {
			s := server.New(server.Options{})
			defer s.Close(context.Background())
			mux := http.NewServeMux()
			mux.Handle(path, s.HocuspocusHandler())
			h := httptest.NewServer(mux)
			defer h.Close()
			c := hpDial(t, "ws"+strings.TrimPrefix(h.URL, "http")+path)
			c.auth(t, "doc", "", syncpkg.ScopeReadWrite)
		})
	}
	url, _ := startTestServer(t, server.Options{})
	for n := 1; n <= 12; n++ {
		c := hpDial(t, url+"/wrong")
		// Drain the y-websocket server's initial SyncStep1.
		if _, _, err := c.ws.Read(c.ctx); err != nil {
			t.Fatal(err)
		}
		c.send(t, strings.Repeat("d", n), syncpkg.EncodeAuthToken("ok"))
		_, _, err := c.ws.Read(c.ctx)
		if websocket.CloseStatus(err) != 1002 || !strings.Contains(err.Error(), "Hocuspocus endpoint") {
			t.Fatalf("length %d: %v", n, err)
		}
	}
	c := hpDial(t, url)
	if err := c.ws.Write(c.ctx, websocket.MessageBinary, syncpkg.EncodeSyncStep1([]byte{0})); err != nil {
		t.Fatal(err)
	}
	_, _, err := c.ws.Read(c.ctx)
	if websocket.CloseStatus(err) != 1008 {
		t.Fatal(err)
	}
}

func TestHocuspocusAwarenessBackplaneAndSweep(t *testing.T) {
	hub := backplane.NewMemory()
	defer hub.Close()
	aURL, _ := startTestServer(t, server.Options{Backplane: hub.Conn(), AwarenessTimeout: 100 * time.Millisecond})
	bURL, _ := startTestServer(t, server.Options{Backplane: hub.Conn(), AwarenessTimeout: 100 * time.Millisecond})
	a := hpDial(t, aURL)
	b := hpDial(t, bURL)
	a.auth(t, "room\x00a", "", syncpkg.ScopeReadWrite)
	b.auth(t, "room\x00b", "", syncpkg.ScopeReadWrite)
	a.send(t, "room\x00a", syncpkg.EncodeSyncUpdate(hpUpdate(t, "relayed")))
	a.want(t, "room\x00a", syncpkg.MessageSyncStatus)
	a.want(t, "room\x00a", syncpkg.MessageSync)
	if f := b.want(t, "room\x00b", syncpkg.MessageSync); fmt.Sprint(hpValues(t, f.Payload)) != "[relayed]" {
		t.Fatal(f)
	}
	aw := awareness.New(41)
	aw.SetLocalState([]byte(`{"name":"peer"}`))
	a.send(t, "room\x00a", syncpkg.EncodeAwareness(aw.Encode([]uint64{41})))
	a.want(t, "room\x00a", syncpkg.MessageAwareness)
	f := b.want(t, "room\x00b", syncpkg.MessageAwareness)
	peer := awareness.New(42)
	if _, err := peer.Apply(f.Payload, "test"); err != nil {
		t.Fatal(err)
	}
	if len(peer.States()) != 1 {
		t.Fatal(peer.States())
	}
	// Periodic sweep broadcasts must retain the recipient's exact key too.
	f = b.want(t, "room\x00b", syncpkg.MessageAwareness)
	if _, err := peer.Apply(f.Payload, "test"); err != nil {
		t.Fatal(err)
	}
	if len(peer.States()) != 0 {
		t.Fatal("sweep did not remove presence")
	}
}

func TestHocuspocusCloseRemovesOnlyItsPresence(t *testing.T) {
	url, _ := startTestServer(t, server.Options{})
	c := hpDial(t, url)
	c.auth(t, "room\x00a", "", syncpkg.ScopeReadWrite)
	c.auth(t, "room\x00b", "", syncpkg.ScopeReadWrite)
	for _, id := range []uint64{41, 42} {
		key := "room\x00a"
		if id == 42 {
			key = "room\x00b"
		}
		aw := awareness.New(id)
		aw.SetLocalState([]byte(`{"present":true}`))
		c.send(t, key, syncpkg.EncodeAwareness(aw.Encode([]uint64{id})))
		for range 2 {
			_, f := c.read(t)
			if f.Type != syncpkg.MessageAwareness {
				t.Fatal(f)
			}
		}
	}
	c.send(t, "room\x00a", []byte{7})
	f := c.want(t, "room\x00b", syncpkg.MessageAwareness)
	peer := awareness.New(43)
	if _, err := peer.Apply(f.Payload, "test"); err != nil {
		t.Fatal(err)
	}
	if len(peer.States()) != 0 {
		t.Fatal("close did not send a tombstone")
	}
	c.want(t, "room\x00a", syncpkg.MessageClose)
	// A newly admitted key gets B's live snapshot, without A.
	c.auth(t, "room\x00c", "", syncpkg.ScopeReadWrite)
	f = c.want(t, "room\x00c", syncpkg.MessageAwareness)
	if _, err := peer.Apply(f.Payload, "test"); err != nil {
		t.Fatal(err)
	}
	states := peer.States()
	if _, ok := states[42]; !ok || len(states) != 1 {
		t.Fatal(states)
	}
}

func TestHocuspocusMalformedAndUnknownSync(t *testing.T) {
	url, s := startTestServer(t, server.Options{})
	c := hpDial(t, url)
	c.auth(t, "a", "", syncpkg.ScopeReadWrite)
	c.auth(t, "b", "", syncpkg.ScopeReadWrite)
	c.send(t, "a", []byte{0, 99})
	c.want(t, "a", syncpkg.MessageClose)
	hpSync(t, c, "b")
	c.send(t, "b", []byte{0, 0, 128})
	_, _, err := c.ws.Read(c.ctx)
	if websocket.CloseStatus(err) != 4401 {
		t.Fatal(err)
	}
	hpAwaitStats(t, s, 0, 0)
}

func TestHocuspocusPanicReleasesOtherKeys(t *testing.T) {
	var disconnects atomic.Int32
	url, s := startTestServer(t, server.Options{
		OnConnect: func(_, name string, _ *http.Request) error {
			if name == "panic" {
				panic("hook panic")
			}
			return nil
		},
		OnDisconnect: func(_, _ string) { disconnects.Add(1) },
	})
	c := hpDial(t, url)
	c.auth(t, "accepted", "", syncpkg.ScopeReadWrite)
	c.send(t, "panic", syncpkg.EncodeAuthToken(""))
	if _, _, err := c.ws.Read(c.ctx); err == nil {
		t.Fatal("panic left socket alive")
	}
	hpAwaitStats(t, s, 0, 0)
	if disconnects.Load() != 1 {
		t.Fatal(disconnects.Load())
	}
}

func TestHocuspocusAuthDeadlineEndsBeforeQueueDrain(t *testing.T) {
	url, _ := startTestServer(t, server.Options{
		AuthTimeout:    100 * time.Millisecond,
		IdleTimeout:    time.Second,
		OnAuthenticate: func(_, _ string) error { return nil },
		OnStateless:    func(_, _ string) { time.Sleep(150 * time.Millisecond) },
	})
	c := hpDial(t, url)
	c.send(t, "room", syncpkg.EncodeStateless("queued"))
	c.auth(t, "room", "", syncpkg.ScopeReadWrite)
	hpSync(t, c, "room")
}
