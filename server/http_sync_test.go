package server_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Deln0r/ygo"
	"github.com/Deln0r/ygo/internal/doc"
	"github.com/Deln0r/ygo/internal/encoding"
	"github.com/Deln0r/ygo/internal/memstore"
	syncpkg "github.com/Deln0r/ygo/internal/sync"
	"github.com/Deln0r/ygo/internal/types"
	"github.com/Deln0r/ygo/server"
	"github.com/Deln0r/ygo/server/backplane"
)

// openHTTP lets every HTTP request through.
func openHTTP(*http.Request, string, bool) error { return nil }

// startHTTPServer serves WebSocket clients at / and HTTPSyncHandler at
// /sync/: ws://host/<name> and http://host/sync/<name> are one document.
func startHTTPServer(t *testing.T, opts server.Options) (wsURL, syncURL string, s *server.Server) {
	t.Helper()
	s = server.New(opts)
	mux := http.NewServeMux()
	mux.Handle("/", s.Handler())
	mux.Handle("/sync/", s.HTTPSyncHandler())
	hs := httptest.NewServer(mux)
	t.Cleanup(func() {
		hs.Close()
		_ = s.Close(context.Background())
	})
	u, _ := url.Parse(hs.URL)
	return "ws://" + u.Host, hs.URL + "/sync/", s
}

type httpResult struct {
	status int
	header http.Header
	body   []byte
}

func httpDo(t *testing.T, method, u string, body []byte, header map[string]string) httpResult {
	t.Helper()
	req, err := http.NewRequest(method, u, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return httpResult{resp.StatusCode, resp.Header, b}
}

func svParam(sv []byte) string { return "?sv=" + base64.StdEncoding.EncodeToString(sv) }

// stepTwo is what a y-websocket client gets as SyncStep2 for sv.
func stepTwo(t *testing.T, wsURL, name string, sv []byte) []byte {
	t.Helper()
	c := dialClient(t, wsURL, name, 99)
	defer c.close()
	c.write(t, syncpkg.EncodeSyncStep1(sv))
	return c.readUntil(t, func(f *syncpkg.Frame) bool {
		return f.Type == syncpkg.MessageSync && f.SyncSub == syncpkg.SyncStep2
	}).Payload
}

// TestHTTPSync_RefusedWithoutHook: without OnHTTPRequest every request is
// 403 and nothing is loaded.
func TestHTTPSync_RefusedWithoutHook(t *testing.T) {
	var loads atomic.Int64
	_, syncURL, s := startHTTPServer(t, server.Options{OnLoadDocument: func(string) ([]byte, error) {
		loads.Add(1)
		return nil, nil
	}})
	if r := httpDo(t, "GET", syncURL+"doc", nil, nil); r.status != http.StatusForbidden {
		t.Fatalf("GET %d", r.status)
	}
	if r := httpDo(t, "POST", syncURL+"doc", hpUpdate(t, "x"), nil); r.status != http.StatusForbidden {
		t.Fatalf("POST %d", r.status)
	}
	if loads.Load() != 0 || s.Stats().Documents != 0 {
		t.Fatalf("loaded %d, stats %+v", loads.Load(), s.Stats())
	}
}

// TestHTTPSync_GetIsSyncStep2: GET returns, byte for byte, what a
// y-websocket client's sync step 2 at the same state vector carries, for
// every form of the sv parameter, with the server's state vector and an
// ETag; v=2 returns the V2 form.
func TestHTTPSync_GetIsSyncStep2(t *testing.T) {
	store := memstore.New()
	_ = store.StoreUpdate(context.Background(), "doc", hpUpdate(t, "stored"))
	wsURL, syncURL, _ := startHTTPServer(t, server.Options{Store: store, OnHTTPRequest: openHTTP})
	c := dialClient(t, wsURL, "doc", 21)
	defer c.close()
	syncClient(t, c)
	c.write(t, syncpkg.EncodeSyncUpdate(hpUpdate(t, "live")))
	awaitContains(t, c, "live")

	empty := []byte{0}
	want := stepTwo(t, wsURL, "doc", empty)
	for _, q := range []string{"", "?sv=", "?sv=AA==", "?sv=AA", "?sv=AA%3D%3D"} {
		r := httpDo(t, "GET", syncURL+"doc"+q, nil, nil)
		if r.status != 200 || !bytes.Equal(r.body, want) {
			t.Fatalf("GET %q: %d %x, want %x", q, r.status, r.body, want)
		}
		if r.header.Get("ETag") == "" || r.header.Get(server.StateVectorHeader) == "" || r.header.Get("Content-Type") != "application/octet-stream" {
			t.Fatalf("GET %q headers %v", q, r.header)
		}
	}
	// Against a state vector inside the document, in both base64 alphabets.
	full := httpDo(t, "GET", syncURL+"doc", nil, nil)
	svRaw, _ := base64.StdEncoding.DecodeString(full.header.Get(server.StateVectorHeader))
	partial := encoding.EncodeStateVector(map[uint64]uint64{21: 0}, nil)
	for _, sv := range [][]byte{svRaw, partial} {
		want := stepTwo(t, wsURL, "doc", sv)
		for _, enc := range []string{base64.StdEncoding.EncodeToString(sv), base64.RawURLEncoding.EncodeToString(sv)} {
			if r := httpDo(t, "GET", syncURL+"doc?sv="+url.QueryEscape(enc), nil, nil); !bytes.Equal(r.body, want) {
				t.Fatalf("GET sv %x (%s): %x, want %x", sv, enc, r.body, want)
			}
		}
	}
	// V2.
	r := httpDo(t, "GET", syncURL+"doc?v=2", nil, nil)
	d := doc.NewDoc()
	if err := encoding.ApplyUpdateV2(d, r.body); err != nil {
		t.Fatalf("v=2 body: %v", err)
	}
	if got := types.NewArray(d.Branch("items")).ToSlice(); !containsAll(got, "stored", "live") {
		t.Fatalf("v=2 holds %v", got)
	}
	// URL-safe base64 with its own characters ('-', '_').
	for c := uint64(1); ; c++ {
		sv := encoding.EncodeStateVector(map[uint64]uint64{c: 1}, nil)
		enc := base64.RawURLEncoding.EncodeToString(sv)
		if !strings.ContainsAny(enc, "-_") {
			continue
		}
		if r := httpDo(t, "GET", syncURL+"doc?sv="+enc, nil, nil); r.status != 200 || !bytes.Equal(r.body, stepTwo(t, wsURL, "doc", sv)) {
			t.Fatalf("URL-safe sv %s: %d", enc, r.status)
		}
		break
	}
	// With an update that waits for a missing one: the pending buffer is in
	// the response, as in a sync step 2.
	waiting := doc.NewDocWithOptions(doc.Options{ClientID: 22})
	warr := types.NewArray(waiting.Branch("items"))
	txn := waiting.WriteTxn()
	warr.Push(txn, "first")
	txn.Commit()
	wsv := ygo.EncodeStateVector(waiting)
	txn = waiting.WriteTxn()
	warr.Push(txn, "second")
	txn.Commit()
	later, _ := ygo.EncodeDiff(waiting, wsv)
	if r := httpDo(t, "POST", syncURL+"doc", later, nil); r.status != 204 {
		t.Fatalf("POST pending %d", r.status)
	}
	if want, r := stepTwo(t, wsURL, "doc", empty), httpDo(t, "GET", syncURL+"doc", nil, nil); !bytes.Equal(r.body, want) {
		t.Fatalf("with pending: %x, want %x", r.body, want)
	}
	for _, bad := range []string{"?sv=!!", "?sv=AAAA", "?sv=AA&sv=AA", "?v=3", "?sv=" + strings.Repeat("A", 70000)} {
		if r := httpDo(t, "GET", syncURL+"doc"+bad, nil, nil); r.status != http.StatusBadRequest {
			t.Fatalf("GET %.20q: %d, want 400", bad, r.status)
		}
	}
}

// TestHTTPSync_PostReachesEveryone: a POSTed update reaches y-websocket and
// Hocuspocus clients and another instance's clients, is stored once and
// reported to OnChange once, as a client's update; with sv the response is
// the document's update for that state vector after the write.
func TestHTTPSync_PostReachesEveryone(t *testing.T) {
	hub := backplane.NewMemory()
	t.Cleanup(func() { _ = hub.Close() })
	store := memstore.New()
	var mu sync.Mutex
	var changes [][]byte
	wsURL, syncURL, _ := startHTTPServer(t, server.Options{Store: store, Backplane: hub.Conn(), OnHTTPRequest: openHTTP,
		OnChange: func(_ string, u []byte) {
			mu.Lock()
			changes = append(changes, append([]byte(nil), u...))
			mu.Unlock()
		}})
	wsB, _ := startTestServer(t, server.Options{Backplane: hub.Conn()})
	y := dialClient(t, wsURL, "doc", 31)
	defer y.close()
	syncClient(t, y)
	hp := hpDial(t, wsURL)
	hp.auth(t, "doc", "", syncpkg.ScopeReadWrite)
	hpSync(t, hp, "doc")
	other := dialClient(t, wsB, "doc", 32)
	defer other.close()
	syncClient(t, other)

	update := hpUpdate(t, "posted")
	if r := httpDo(t, "POST", syncURL+"doc", update, nil); r.status != http.StatusNoContent || r.header.Get("ETag") == "" || r.header.Get(server.StateVectorHeader) == "" {
		t.Fatalf("POST %d %v", r.status, r.header)
	}
	awaitContains(t, y, "posted")
	if f := hp.want(t, "doc", syncpkg.MessageSync); !bytes.Equal(f.Payload, update) {
		t.Fatalf("Hocuspocus got %x", f.Payload)
	}
	awaitContains(t, other, "posted")
	if got := storeItems(t, store, "doc"); fmt.Sprint(got) != "[posted]" {
		t.Fatalf("stored %v", got)
	}
	mu.Lock()
	if len(changes) != 1 || !bytes.Equal(changes[0], update) {
		t.Fatalf("OnChange %d", len(changes))
	}
	mu.Unlock()

	// Push and pull in one request: the reply holds what the poster lacks.
	y.write(t, syncpkg.EncodeSyncUpdate(hpUpdate(t, "from-ws")))
	awaitContains(t, y, "from-ws")
	mine := doc.NewDocWithOptions(doc.Options{ClientID: 33})
	mineItems := types.NewArray(mine.Branch("items"))
	txn := mine.WriteTxn()
	mineItems.Push(txn, "mine")
	txn.Commit()
	r := httpDo(t, "POST", syncURL+"doc"+svParam(ygo.EncodeStateVector(mine)), encoding.EncodeStateAsUpdate(mine), nil)
	if r.status != http.StatusOK {
		t.Fatalf("POST with sv: %d %s", r.status, r.body)
	}
	if err := encoding.ApplyUpdate(mine, r.body); err != nil {
		t.Fatal(err)
	}
	if got := types.NewArray(mine.Branch("items")).ToSlice(); !containsAll(got, "posted", "from-ws", "mine") {
		t.Fatalf("after one round trip %v", got)
	}
}

// TestHTTPSync_ETag: a GET with the current ETag is 304; any change makes
// it 200 again: an insert, a delete with no new struct, an update that
// only waits, a reload of the document.
func TestHTTPSync_ETag(t *testing.T) {
	store := memstore.New()
	wsURL, syncURL, s := startHTTPServer(t, server.Options{Store: store, OnHTTPRequest: openHTTP})
	c := dialClient(t, wsURL, "doc", 41)
	defer c.close()
	syncClient(t, c)
	get := func() httpResult { return httpDo(t, "GET", syncURL+"doc", nil, nil) }
	tag := get().header.Get("ETag")
	expect := func(what string, status int) {
		t.Helper()
		r := httpDo(t, "GET", syncURL+"doc", nil, map[string]string{"If-None-Match": tag})
		if r.status != status {
			t.Fatalf("%s: %d, want %d", what, r.status, status)
		}
		if status == 200 {
			tag = r.header.Get("ETag")
		}
	}
	expect("unchanged", 304)
	// An insert, then deleting it (no new struct).
	writer := doc.NewDocWithOptions(doc.Options{ClientID: 42})
	arr := types.NewArray(writer.Branch("items"))
	txn := writer.WriteTxn()
	arr.Push(txn, "a")
	txn.Commit()
	if r := httpDo(t, "POST", syncURL+"doc", encoding.EncodeStateAsUpdate(writer), nil); r.status != 204 {
		t.Fatalf("POST %d", r.status)
	}
	expect("after an insert", 200)
	expect("unchanged again", 304)
	sv := ygo.EncodeStateVector(writer)
	txn = writer.WriteTxn()
	arr.Delete(txn, 0, 1)
	txn.Commit()
	del, _ := ygo.EncodeDiff(writer, sv)
	c.write(t, syncpkg.EncodeSyncUpdate(del))
	waitFor(t, 5*time.Second, func() bool { return get().header.Get("ETag") != tag })
	expect("after a delete", 200)
	// An update that waits for a missing one changes only the pending buffer.
	waiting := doc.NewDocWithOptions(doc.Options{ClientID: 43})
	warr := types.NewArray(waiting.Branch("items"))
	txn = waiting.WriteTxn()
	warr.Push(txn, "first")
	txn.Commit()
	wsv := ygo.EncodeStateVector(waiting)
	txn = waiting.WriteTxn()
	warr.Push(txn, "second")
	txn.Commit()
	later, _ := ygo.EncodeDiff(waiting, wsv)
	if r := httpDo(t, "POST", syncURL+"doc", later, nil); r.status != 204 {
		t.Fatalf("POST pending %d", r.status)
	}
	expect("after a pending update", 200)
	if r := httpDo(t, "GET", syncURL+"doc", nil, map[string]string{"If-None-Match": "W/" + tag}); r.status != 304 {
		t.Fatalf("weak tag: %d", r.status)
	}
	// A reload is another copy: its tags are new. Without DocIdleTimeout a
	// document no client holds is loaded for each request, so each is 200.
	c.close()
	waitFor(t, 5*time.Second, func() bool { return s.Stats().Documents == 0 })
	expect("after a reload", 200)
	expect("reloaded again", 200)
}

// TestHTTPSync_ETagBackplane: an update another instance publishes changes
// the ETag.
func TestHTTPSync_ETagBackplane(t *testing.T) {
	hub := backplane.NewMemory()
	t.Cleanup(func() { _ = hub.Close() })
	wsA, syncURL, _ := startHTTPServer(t, server.Options{Backplane: hub.Conn(), OnHTTPRequest: openHTTP})
	wsB, _ := startTestServer(t, server.Options{Backplane: hub.Conn()})
	holder := dialClient(t, wsA, "doc", 81)
	defer holder.close()
	syncClient(t, holder)
	tag := httpDo(t, "GET", syncURL+"doc", nil, nil).header.Get("ETag")
	other := dialClient(t, wsB, "doc", 82)
	defer other.close()
	syncClient(t, other)
	other.write(t, syncpkg.EncodeSyncUpdate(hpUpdate(t, "elsewhere")))
	awaitContains(t, holder, "elsewhere")
	if r := httpDo(t, "GET", syncURL+"doc", nil, map[string]string{"If-None-Match": tag}); r.status != 200 {
		t.Fatalf("after a foreign update: %d", r.status)
	}
}

// TestHTTPSync_ETagParked: with DocIdleTimeout a polled document stays
// resident between requests, and an unchanged one is 304.
func TestHTTPSync_ETagParked(t *testing.T) {
	store := memstore.New()
	_ = store.StoreUpdate(context.Background(), "doc", hpUpdate(t, "stored"))
	_, syncURL, s := startHTTPServer(t, server.Options{Store: store, DocIdleTimeout: time.Hour, OnHTTPRequest: openHTTP})
	tag := httpDo(t, "GET", syncURL+"doc", nil, nil).header.Get("ETag")
	for i := 0; i < 3; i++ {
		if r := httpDo(t, "GET", syncURL+"doc", nil, map[string]string{"If-None-Match": tag}); r.status != 304 || len(r.body) != 0 {
			t.Fatalf("poll %d: %d", i, r.status)
		}
	}
	if st := s.Stats(); st.IdleDocuments != 1 {
		t.Fatalf("%+v", st)
	}
}

// TestHTTPSync_Access: OnHTTPRequest refuses before any load, with its
// HTTPError's status; ReadOnly refuses POST and lets GET through; a browser
// Origin must be the host or match OriginPatterns.
func TestHTTPSync_Access(t *testing.T) {
	var loads atomic.Int64
	_, syncURL, _ := startHTTPServer(t, server.Options{
		OriginPatterns: []string{"app.example.com", "https://app2.example.com"},
		OnLoadDocument: func(string) ([]byte, error) { loads.Add(1); return nil, nil },
		OnHTTPRequest: func(r *http.Request, name string, write bool) error {
			switch {
			case r.Header.Get("Authorization") == "":
				return &server.HTTPError{Status: http.StatusUnauthorized, Message: "who are you"}
			case write && name == "locked":
				return errors.New("no writes here")
			}
			return nil
		},
		ReadOnly: func(name string, r *http.Request) bool { return name == "viewer" },
	})
	auth := map[string]string{"Authorization": "Bearer t"}
	if r := httpDo(t, "GET", syncURL+"doc", nil, nil); r.status != 401 || !strings.Contains(string(r.body), "who are you") {
		t.Fatalf("no auth: %d %s", r.status, r.body)
	}
	if r := httpDo(t, "POST", syncURL+"locked", hpUpdate(t, "x"), auth); r.status != 403 {
		t.Fatalf("locked POST: %d", r.status)
	}
	if loads.Load() != 0 {
		t.Fatalf("refused requests loaded %d documents", loads.Load())
	}
	if r := httpDo(t, "POST", syncURL+"viewer", hpUpdate(t, "x"), auth); r.status != 403 {
		t.Fatalf("read-only POST: %d", r.status)
	}
	if r := httpDo(t, "GET", syncURL+"viewer", nil, auth); r.status != 200 {
		t.Fatalf("read-only GET: %d", r.status)
	}
	for origin, status := range map[string]int{"https://evil.example": 403, "https://app.example.com": 200, "https://app2.example.com": 200, "http://app2.example.com": 403} {
		h := map[string]string{"Authorization": "Bearer t", "Origin": origin}
		if r := httpDo(t, "GET", syncURL+"doc", nil, h); r.status != status {
			t.Fatalf("Origin %s: %d, want %d", origin, r.status, status)
		}
	}
	u, _ := url.Parse(syncURL)
	if r := httpDo(t, "GET", syncURL+"doc", nil, map[string]string{"Authorization": "Bearer t", "Origin": "http://" + u.Host}); r.status != 200 {
		t.Fatalf("same-host Origin: %d", r.status)
	}
}

// TestHTTPSync_Refusals: methods, media type, body size, invalid and
// trailing bytes, V2 POST, names, the connection and document caps.
func TestHTTPSync_Refusals(t *testing.T) {
	wsURL, syncURL, _ := startHTTPServer(t, server.Options{OnHTTPRequest: openHTTP, ReadLimit: 64, MaxConns: 2, MaxDocs: 1})
	if r := httpDo(t, "PUT", syncURL+"doc", nil, nil); r.status != 405 || r.header.Get("Allow") != "GET, POST" {
		t.Fatalf("PUT %d %v", r.status, r.header)
	}
	if r := httpDo(t, "POST", syncURL+"doc", hpUpdate(t, "x"), map[string]string{"Content-Type": "text/plain"}); r.status != 415 {
		t.Fatalf("text/plain %d", r.status)
	}
	if r := httpDo(t, "POST", syncURL+"doc", hpUpdate(t, strings.Repeat("x", 100)), nil); r.status != 413 {
		t.Fatalf("large %d", r.status)
	}
	if r := httpDo(t, "POST", syncURL+"doc", append(hpUpdate(t, "x"), 0), nil); r.status != 400 {
		t.Fatalf("trailing byte %d", r.status)
	}
	if r := httpDo(t, "POST", syncURL+"doc", []byte{9, 9, 9}, nil); r.status != 400 {
		t.Fatalf("garbage %d", r.status)
	}
	if r := httpDo(t, "POST", syncURL+"doc?v=2", hpUpdate(t, "x"), nil); r.status != 400 {
		t.Fatalf("V2 POST %d", r.status)
	}
	for _, name := range []string{"", "a%00b", strings.Repeat("n", 1025)} {
		if r := httpDo(t, "GET", syncURL+name, nil, nil); r.status != 400 {
			t.Fatalf("name %.10q: %d", name, r.status)
		}
	}
	// The document cap: one document held by a client, a second refused.
	c := dialClient(t, wsURL, "held", 51)
	defer c.close()
	syncClient(t, c)
	if r := httpDo(t, "POST", syncURL+"other", hpUpdate(t, "x"), nil); r.status != 503 {
		t.Fatalf("MaxDocs %d", r.status)
	}
	// The connection cap: two clients hold both slots.
	c2 := dialClient(t, wsURL, "held", 52)
	defer c2.close()
	syncClient(t, c2)
	if r := httpDo(t, "GET", syncURL+"held", nil, nil); r.status != 503 || r.header.Get("Retry-After") == "" {
		t.Fatalf("MaxConns %d", r.status)
	}
}

// TestHTTPSync_ReadsDoNotKeepDocuments: GETs of made-up names do not keep
// documents resident (so they cannot fill MaxDocs or the idle documents); a
// GET of a stored document settles it as a visit does; a GET of a parked
// document leaves it parked, without another flush.
func TestHTTPSync_ReadsDoNotKeepDocuments(t *testing.T) {
	store := &countingStore{Store: memstore.New()}
	_ = store.StoreUpdate(context.Background(), "real", hpUpdate(t, "stored"))
	_, syncURL, s := startHTTPServer(t, server.Options{Store: store, DocIdleTimeout: time.Hour, MaxDocs: 2, OnHTTPRequest: openHTTP})
	for i := 0; i < 10; i++ {
		if r := httpDo(t, "GET", fmt.Sprintf("%smade-up-%d", syncURL, i), nil, nil); r.status != 200 {
			t.Fatalf("GET %d: %d", i, r.status)
		}
	}
	if st := s.Stats(); st.Documents != 0 {
		t.Fatalf("made-up names stayed: %+v", st)
	}
	if store.flushes.Load() != 0 {
		t.Fatalf("made-up names flushed %d times", store.flushes.Load())
	}
	if r := httpDo(t, "GET", syncURL+"real", nil, nil); r.status != 200 {
		t.Fatal(r.status)
	}
	if st := s.Stats(); st.IdleDocuments != 1 {
		t.Fatalf("a stored document was not parked: %+v", st)
	}
	flushes := store.flushes.Load()
	for i := 0; i < 3; i++ {
		httpDo(t, "GET", syncURL+"real", nil, nil)
	}
	if st := s.Stats(); st.IdleDocuments != 1 || store.flushes.Load() != flushes {
		t.Fatalf("reads of a parked document: %+v, %d flushes (was %d)", st, store.flushes.Load(), flushes)
	}
}

type countingStore struct {
	*memstore.Store
	flushes  atomic.Int64
	failNext atomic.Bool
}

func (c *countingStore) Flush(ctx context.Context, name string) error {
	c.flushes.Add(1)
	return c.Store.Flush(ctx, name)
}

func (c *countingStore) StoreUpdate(ctx context.Context, name string, u []byte) error {
	if c.failNext.Swap(false) {
		return errors.New("disk full")
	}
	return c.Store.StoreUpdate(ctx, name, u)
}

// TestHTTPSync_StoreError: an update applied and sent but not stored is a
// 500 that says so; the clients still have it.
func TestHTTPSync_StoreError(t *testing.T) {
	store := &countingStore{Store: memstore.New()}
	wsURL, syncURL, _ := startHTTPServer(t, server.Options{Store: store, OnHTTPRequest: openHTTP})
	c := dialClient(t, wsURL, "doc", 61)
	defer c.close()
	syncClient(t, c)
	store.failNext.Store(true)
	r := httpDo(t, "POST", syncURL+"doc", hpUpdate(t, "unstored"), nil)
	if r.status != 500 || !strings.Contains(string(r.body), "not stored") {
		t.Fatalf("%d %s", r.status, r.body)
	}
	awaitContains(t, c, "unstored")
}

// TestServerWrite_RetainsOnlyWhatAddsSomething: without a Store a document
// stays resident after an update that added something, and not after one
// that added nothing, for HTTP and server writes alike.
func TestServerWrite_RetainsOnlyWhatAddsSomething(t *testing.T) {
	_, syncURL, s := startHTTPServer(t, server.Options{OnHTTPRequest: openHTTP})
	empty := []byte{0, 0}
	if r := httpDo(t, "POST", syncURL+"a", empty, nil); r.status != 204 {
		t.Fatal(r.status)
	}
	if err := s.ApplyUpdate(context.Background(), "b", empty); err != nil {
		t.Fatal(err)
	}
	if st := s.Stats(); st.Documents != 0 {
		t.Fatalf("empty updates retained: %+v", st)
	}
	if r := httpDo(t, "POST", syncURL+"a", hpUpdate(t, "x"), nil); r.status != 204 {
		t.Fatal(r.status)
	}
	if st := s.Stats(); st.Documents != 1 {
		t.Fatalf("a written document was not retained: %+v", st)
	}
}

// TestHTTPSync_Concurrent: HTTP posters and WebSocket writers on one
// document converge.
func TestHTTPSync_Concurrent(t *testing.T) {
	wsURL, syncURL, s := startHTTPServer(t, server.Options{OnHTTPRequest: openHTTP})
	c := dialClient(t, wsURL, "doc", 71)
	defer c.close()
	syncClient(t, c)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			d := doc.NewDocWithOptions(doc.Options{ClientID: uint64(100 + i)})
			items := types.NewArray(d.Branch("items"))
			for j := 0; j < 5; j++ {
				sv := ygo.EncodeStateVector(d)
				txn := d.WriteTxn()
				items.Push(txn, fmt.Sprintf("h%d-%d", i, j))
				txn.Commit()
				u, _ := ygo.EncodeDiff(d, sv)
				resp, err := http.Post(syncURL+"doc", "application/octet-stream", bytes.NewReader(u))
				if err != nil {
					t.Error(err)
					return
				}
				resp.Body.Close()
				if resp.StatusCode != 204 {
					t.Errorf("POST %d", resp.StatusCode)
				}
				httpDo(t, "GET", syncURL+"doc", nil, nil)
			}
		}(i)
	}
	wg.Wait()
	d, err := s.OpenDocument(context.Background(), "doc")
	if err != nil {
		t.Fatal(err)
	}
	state, _ := d.State()
	d.Close()
	if got := stateItems(t, state); len(got) != 40 {
		t.Fatalf("%d items", len(got))
	}
}

// TestHTTPSync_SlotBeforeBody: a POST past MaxConns is refused before its
// body is read: the response comes while most of the announced body has not
// been sent, and the connection is closed.
func TestHTTPSync_SlotBeforeBody(t *testing.T) {
	wsURL, syncURL, _ := startHTTPServer(t, server.Options{OnHTTPRequest: openHTTP, MaxConns: 1})
	c := dialClient(t, wsURL, "doc", 91)
	defer c.close()
	syncClient(t, c)
	u, _ := url.Parse(syncURL)
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(conn, "POST /sync/doc HTTP/1.1\r\nHost: %s\r\nContent-Type: application/octet-stream\r\nContent-Length: 1000000\r\n\r\n\x01\x01", u.Host)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("no response before the body: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 503 || !resp.Close {
		t.Fatalf("%d, close %v", resp.StatusCode, resp.Close)
	}
}

// TestHTTPSync_PanickingOnChangeLetsGo: a POST whose OnChange panics lets
// the document go, so CloseDocument and Close finish.
func TestHTTPSync_PanickingOnChangeLetsGo(t *testing.T) {
	_, syncURL, s := startHTTPServer(t, server.Options{Store: memstore.New(), OnHTTPRequest: openHTTP,
		OnChange: func(string, []byte) { panic("adopter bug") }})
	req, _ := http.NewRequest("POST", syncURL+"doc", bytes.NewReader(hpUpdate(t, "x")))
	req.Header.Set("Content-Type", "application/octet-stream")
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.CloseDocument(ctx, "doc", true); err != nil && !errors.Is(err, server.ErrDocumentNotFound) {
		t.Fatalf("CloseDocument: %v", err)
	}
	if err := s.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
