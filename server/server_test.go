package server_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Deln0r/ygo/internal/awareness"
	"github.com/Deln0r/ygo/internal/doc"
	"github.com/Deln0r/ygo/internal/encoding"
	syncpkg "github.com/Deln0r/ygo/internal/sync"
	"github.com/Deln0r/ygo/internal/types"
	"github.com/Deln0r/ygo/persist/sqlite"
	"github.com/Deln0r/ygo/server"
)

// startTestServer wires a server.Server into httptest. Returns the
// WS URL prefix (ws://host) plus a cleanup. The path component
// becomes docName per the default DocNameFn.
func startTestServer(t *testing.T, opts server.Options) (string, *server.Server) {
	t.Helper()
	srv := server.New(opts)
	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		httpSrv.Close()
		_ = srv.Close(context.Background())
	})
	u, err := url.Parse(httpSrv.URL)
	if err != nil {
		t.Fatal(err)
	}
	wsURL := "ws://" + u.Host
	return wsURL, srv
}

// wsClient wraps coder/websocket Conn with simple Read/Write helpers
// that hide context plumbing. Tests use it like a channel pair.
type wsClient struct {
	conn   *websocket.Conn
	ctx    context.Context
	cancel context.CancelFunc

	doc       *doc.Doc
	awareness *awareness.Awareness
}

func dialClient(t *testing.T, wsBaseURL, docName string, clientID uint64) *wsClient {
	t.Helper()
	dialURL := strings.TrimRight(wsBaseURL, "/") + "/" + docName

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	c, _, err := websocket.Dial(ctx, dialURL, nil)
	if err != nil {
		cancel()
		t.Fatalf("ws dial: %v", err)
	}
	return &wsClient{
		conn:      c,
		ctx:       ctx,
		cancel:    cancel,
		doc:       doc.NewDocWithOptions(doc.Options{ClientID: clientID}),
		awareness: awareness.New(clientID),
	}
}

func (c *wsClient) close() {
	_ = c.conn.Close(websocket.StatusNormalClosure, "test done")
	c.cancel()
}

func (c *wsClient) write(t *testing.T, envelope []byte) {
	t.Helper()
	if err := c.conn.Write(c.ctx, websocket.MessageBinary, envelope); err != nil {
		t.Fatalf("ws write: %v", err)
	}
}

func (c *wsClient) read(t *testing.T) *syncpkg.Frame {
	t.Helper()
	_, raw, err := c.conn.Read(c.ctx)
	if err != nil {
		t.Fatalf("ws read: %v", err)
	}
	frame, _, err := syncpkg.DecodeEnvelope(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return frame
}

// readUntil reads frames until a frame matching match() arrives or
// the context times out. Returns the matching frame.
func (c *wsClient) readUntil(t *testing.T, match func(*syncpkg.Frame) bool) *syncpkg.Frame {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		readCtx, cancel := context.WithTimeout(c.ctx, time.Until(deadline))
		_, raw, err := c.conn.Read(readCtx)
		cancel()
		if err != nil {
			t.Fatalf("ws read: %v", err)
		}
		frame, _, err := syncpkg.DecodeEnvelope(raw)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if match(frame) {
			return frame
		}
	}
	t.Fatalf("readUntil: deadline exceeded")
	return nil
}

func TestServer_TwoClients_SyncUpdateBroadcast(t *testing.T) {
	wsURL, _ := startTestServer(t, server.Options{OriginPatterns: []string{"*"}})

	a := dialClient(t, wsURL, "convergence", 100)
	defer a.close()

	// Read server's initial SyncStep1 + Awareness handshake (empty doc).
	first := a.read(t)
	if first.Type != syncpkg.MessageSync || first.SyncSub != syncpkg.SyncStep1 {
		t.Fatalf("a initial frame = %d/%d, want SyncStep1", first.Type, first.SyncSub)
	}

	// Make an edit on A's local doc and send as SyncUpdate.
	arrA := types.NewArray(a.doc.Branch("items"))
	txnA := a.doc.WriteTxn()
	arrA.Push(txnA, "from-A")
	txnA.Commit()
	update := encoding.EncodeStateAsUpdate(a.doc)
	a.write(t, syncpkg.EncodeSyncUpdate(update))

	// A receives the broadcast back (self-included per port-note gotcha 6).
	a.readUntil(t, func(f *syncpkg.Frame) bool {
		return f.Type == syncpkg.MessageSync && f.SyncSub == syncpkg.SyncUpdate
	})

	// Now connect B; B's initial SyncStep1 from the server must
	// already cover A's edit.
	b := dialClient(t, wsURL, "convergence", 200)
	defer b.close()

	// B reads server's SyncStep1.
	bFirst := b.read(t)
	if bFirst.Type != syncpkg.MessageSync || bFirst.SyncSub != syncpkg.SyncStep1 {
		t.Fatalf("b initial frame = %d/%d, want SyncStep1", bFirst.Type, bFirst.SyncSub)
	}
	// Reply with empty SV — server should send full state including A's edit.
	b.write(t, syncpkg.EncodeSyncStep1(encoding.EncodeStateVector(map[uint64]uint64{}, nil)))

	// B reads the SyncStep2 carrying A's content.
	step2 := b.readUntil(t, func(f *syncpkg.Frame) bool {
		return f.Type == syncpkg.MessageSync && f.SyncSub == syncpkg.SyncStep2
	})
	if err := encoding.ApplyUpdate(b.doc, step2.Payload); err != nil {
		t.Fatal(err)
	}

	arrB := types.NewArray(b.doc.Branch("items"))
	if arrB.Len() != 1 {
		t.Fatalf("B's Array Len = %d, want 1", arrB.Len())
	}
	if got := arrB.ToSlice(); got[0] != "from-A" {
		t.Errorf("B's first item = %v, want from-A", got[0])
	}
}

func TestServer_Awareness_BroadcastBetweenClients(t *testing.T) {
	wsURL, _ := startTestServer(t, server.Options{OriginPatterns: []string{"*"}})

	a := dialClient(t, wsURL, "presence", 300)
	defer a.close()
	a.read(t) // initial SyncStep1

	// A announces presence.
	a.awareness.SetLocalState([]byte(`{"name":"Alice"}`))
	a.write(t, syncpkg.EncodeAwareness(a.awareness.Encode([]uint64{300})))

	// A reads its own awareness broadcast back.
	a.readUntil(t, func(f *syncpkg.Frame) bool { return f.Type == syncpkg.MessageAwareness })

	// B connects, queries awareness.
	b := dialClient(t, wsURL, "presence", 400)
	defer b.close()

	// B reads server's initial frames (SyncStep1 + Awareness snapshot
	// since A's presence is already known).
	sawA := false
	for i := 0; i < 5 && !sawA; i++ {
		f := b.read(t)
		if f.Type == syncpkg.MessageAwareness {
			summary, err := b.awareness.Apply(f.Payload, "server")
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := b.awareness.States()[300]; ok {
				sawA = true
				_ = summary
			}
		}
	}
	if !sawA {
		t.Fatal("B did not receive A's awareness in initial handshake")
	}
	if string(b.awareness.States()[300]) != `{"name":"Alice"}` {
		t.Errorf("B's view of A's state = %s, want Alice", b.awareness.States()[300])
	}
}

func TestServer_Persistence_StateSurvivesRestart(t *testing.T) {
	// Round 1: server with a file-backed sqlite store; one client
	// pushes content; disconnect.
	dbPath := filepath.Join(t.TempDir(), "ygo-server.db")
	store1, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	wsURL1, _ := startTestServer(t, server.Options{
		Store:          store1,
		OriginPatterns: []string{"*"},
	})

	a := dialClient(t, wsURL1, "durable", 500)
	a.read(t) // initial SyncStep1

	arr := types.NewArray(a.doc.Branch("items"))
	txn := a.doc.WriteTxn()
	arr.Push(txn, "persisted")
	txn.Commit()
	update := encoding.EncodeStateAsUpdate(a.doc)
	a.write(t, syncpkg.EncodeSyncUpdate(update))
	a.readUntil(t, func(f *syncpkg.Frame) bool {
		return f.Type == syncpkg.MessageSync && f.SyncSub == syncpkg.SyncUpdate
	})

	a.close()
	// Give the server time to flush on last-conn release.
	time.Sleep(100 * time.Millisecond)
	_ = store1.Close()

	// Round 2: brand-new server, same db, new client. Server must
	// load the persisted state on first connect.
	store2, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	wsURL2, _ := startTestServer(t, server.Options{
		Store:          store2,
		OriginPatterns: []string{"*"},
	})

	b := dialClient(t, wsURL2, "durable", 600)
	defer b.close()

	// B sends empty SV; server replies with the persisted state.
	b.read(t) // server's initial SyncStep1
	b.write(t, syncpkg.EncodeSyncStep1(encoding.EncodeStateVector(map[uint64]uint64{}, nil)))

	step2 := b.readUntil(t, func(f *syncpkg.Frame) bool {
		return f.Type == syncpkg.MessageSync && f.SyncSub == syncpkg.SyncStep2
	})
	if err := encoding.ApplyUpdate(b.doc, step2.Payload); err != nil {
		t.Fatal(err)
	}
	arrB := types.NewArray(b.doc.Branch("items"))
	if arrB.Len() != 1 || arrB.ToSlice()[0] != "persisted" {
		t.Errorf("B's array = %v, want [persisted]", arrB.ToSlice())
	}
}

func TestServer_DocNameRouting_IsolatesDocs(t *testing.T) {
	wsURL, _ := startTestServer(t, server.Options{OriginPatterns: []string{"*"}})

	a := dialClient(t, wsURL, "doc-A", 700)
	defer a.close()
	b := dialClient(t, wsURL, "doc-B", 800)
	defer b.close()

	a.read(t)
	b.read(t)

	// A pushes content; B should NOT see it (different doc).
	arr := types.NewArray(a.doc.Branch("items"))
	txn := a.doc.WriteTxn()
	arr.Push(txn, "alpha")
	txn.Commit()
	a.write(t, syncpkg.EncodeSyncUpdate(encoding.EncodeStateAsUpdate(a.doc)))
	a.readUntil(t, func(f *syncpkg.Frame) bool {
		return f.Type == syncpkg.MessageSync && f.SyncSub == syncpkg.SyncUpdate
	})

	// Race-detect: ensure B does not receive A's update within a
	// short window. Use a context with timeout for the read.
	readCtx, cancel := context.WithTimeout(b.ctx, 500*time.Millisecond)
	defer cancel()
	_, _, err := b.conn.Read(readCtx)
	if err == nil {
		t.Error("doc-B received a frame; isolation broken")
	}
	// Timeout error is the success case here.
}

func TestServer_EmptyDocName_Returns400(t *testing.T) {
	wsURL, _ := startTestServer(t, server.Options{OriginPatterns: []string{"*"}})

	// Empty path → empty docName → server should reject with 400.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, resp, err := websocket.Dial(ctx, wsURL+"/", nil)
	if err == nil {
		t.Error("dial succeeded; want failure due to empty docName")
	}
	if resp != nil && resp.StatusCode != 400 {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestServer_ConcurrentEdits_AllConverge(t *testing.T) {
	// Stress: 5 clients each push 3 items concurrently. Server must
	// fan everything out; every client's local doc must eventually
	// see all 15 items.
	const clients = 5
	const perClient = 3

	wsURL, _ := startTestServer(t, server.Options{OriginPatterns: []string{"*"}})

	conns := make([]*wsClient, clients)
	for i := 0; i < clients; i++ {
		conns[i] = dialClient(t, wsURL, "stress", uint64(900+i))
		conns[i].read(t) // initial SyncStep1
	}
	defer func() {
		for _, c := range conns {
			c.close()
		}
	}()

	// Each client pushes items in parallel.
	var wg sync.WaitGroup
	for i, c := range conns {
		i, c := i, c
		wg.Add(1)
		go func() {
			defer wg.Done()
			arr := types.NewArray(c.doc.Branch("items"))
			for j := 0; j < perClient; j++ {
				txn := c.doc.WriteTxn()
				arr.Push(txn, "client")
				_ = i
				_ = j
				txn.Commit()
				update := encoding.EncodeStateAsUpdate(c.doc)
				c.write(t, syncpkg.EncodeSyncUpdate(update))
			}
		}()
	}
	wg.Wait()

	// Read drain — each client should now receive its own + others'
	// updates. We don't assert exact ordering; just that each client
	// converges to len >= clients*perClient.
	//
	// A real test would wait for a quiescence condition; we use a
	// timeout-bounded drain loop instead.
	for _, c := range conns {
		drainDeadline := time.Now().Add(3 * time.Second)
		arr := types.NewArray(c.doc.Branch("items"))
		for arr.Len() < clients*perClient && time.Now().Before(drainDeadline) {
			readCtx, cancel := context.WithTimeout(c.ctx, 200*time.Millisecond)
			_, raw, err := c.conn.Read(readCtx)
			cancel()
			if err != nil {
				continue
			}
			frame, _, decErr := syncpkg.DecodeEnvelope(raw)
			if decErr != nil {
				continue
			}
			if frame.Type == syncpkg.MessageSync &&
				(frame.SyncSub == syncpkg.SyncStep2 || frame.SyncSub == syncpkg.SyncUpdate) {
				_ = encoding.ApplyUpdate(c.doc, frame.Payload)
			}
		}
		if arr.Len() < clients*perClient {
			t.Errorf("client %d: Len = %d, want >= %d", c.doc.ClientID(), arr.Len(), clients*perClient)
		}
	}
}

func TestServer_HocusAuth_DeniesAndCloses4401(t *testing.T) {
	wsURL, _ := startTestServer(t, server.Options{
		OriginPatterns: []string{"*"},
		OnAuthenticate: func(_, token string) error {
			if token != "good-token" {
				return errors.New("invalid token")
			}
			return nil
		},
	})

	// Bad-token client: connects, sends auth, server denies + closes
	// with WS code 4401.
	dialURL := strings.TrimRight(wsURL, "/") + "/auth-doc"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	wsConn, _, err := websocket.Dial(ctx, dialURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer wsConn.Close(websocket.StatusNormalClosure, "test done")

	// Drain the server's initial SyncStep1 + (optional) Awareness
	// snapshot — these are sent before any auth handshake check.
	_, _, _ = wsConn.Read(ctx)

	// Send bad-token auth.
	if err := wsConn.Write(ctx, websocket.MessageBinary,
		syncpkg.EncodeAuthToken("bad-token")); err != nil {
		t.Fatal(err)
	}

	// The client must get AuthPermissionDenied, then the Close envelope,
	// then the WS close with 4401, in that order and nothing between.
	_, raw, err := wsConn.Read(ctx)
	if err != nil {
		t.Fatalf("first message after the bad token: %v, want AuthPermissionDenied", err)
	}
	if f, _, err := syncpkg.DecodeEnvelope(raw); err != nil || f.Type != syncpkg.MessageAuth || f.AuthSub != syncpkg.AuthPermissionDenied {
		t.Fatalf("first message after the bad token = %+v (%v), want AuthPermissionDenied", f, err)
	}
	_, raw, err = wsConn.Read(ctx)
	if err != nil {
		t.Fatalf("second message after the bad token: %v, want the Close envelope", err)
	}
	if f, _, err := syncpkg.DecodeEnvelope(raw); err != nil || f.Type != syncpkg.MessageClose {
		t.Fatalf("second message after the bad token = %+v (%v), want the Close envelope", f, err)
	}
	_, _, err = wsConn.Read(ctx)
	if code := websocket.CloseStatus(err); code != syncpkg.CloseStatusUnauthorized {
		t.Fatalf("after the Close envelope: %v (status %d), want the WS close %d", err, code, syncpkg.CloseStatusUnauthorized)
	}
}

func TestServer_HocusAuth_AcceptsAndProceeds(t *testing.T) {
	wsURL, _ := startTestServer(t, server.Options{
		OriginPatterns: []string{"*"},
		OnAuthenticate: func(_, token string) error {
			if token != "good-token" {
				return errors.New("invalid token")
			}
			return nil
		},
	})

	c := dialClient(t, wsURL, "auth-doc", 9000)
	defer c.close()
	c.read(t) // initial SyncStep1

	c.write(t, syncpkg.EncodeAuthToken("good-token"))
	// Server should reply with Authenticated.
	reply := c.readUntil(t, func(f *syncpkg.Frame) bool {
		return f.Type == syncpkg.MessageAuth
	})
	if reply.AuthSub != syncpkg.AuthAuthenticated {
		t.Errorf("AuthSub = %d, want Authenticated", reply.AuthSub)
	}
}

func TestServer_HocusStateless_BroadcastsAcrossClients(t *testing.T) {
	var serverSeen []string
	wsURL, _ := startTestServer(t, server.Options{
		OriginPatterns: []string{"*"},
		OnStateless: func(_, payload string) {
			serverSeen = append(serverSeen, payload)
		},
	})

	a := dialClient(t, wsURL, "rpc-doc", 9100)
	defer a.close()
	a.read(t) // initial SyncStep1

	b := dialClient(t, wsURL, "rpc-doc", 9101)
	defer b.close()
	b.read(t)

	a.write(t, syncpkg.EncodeBroadcastStateless("ping-1"))
	// Both clients should see the broadcast (self-included).
	for label, c := range map[string]*wsClient{"A": a, "B": b} {
		got := c.readUntil(t, func(f *syncpkg.Frame) bool {
			return f.Type == syncpkg.MessageBroadcastStateless
		})
		if string(got.Payload) != "ping-1" {
			t.Errorf("%s received %q, want ping-1", label, got.Payload)
		}
	}

	if len(serverSeen) != 1 || serverSeen[0] != "ping-1" {
		t.Errorf("server OnStateless saw %v, want [ping-1]", serverSeen)
	}
}
