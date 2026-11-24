package client_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Deln0r/ygo/client"
	"github.com/Deln0r/ygo/internal/doc"
	"github.com/Deln0r/ygo/internal/types"
	"github.com/Deln0r/ygo/server"
)

// httpCounts counts the requests a test server got by method and status.
type httpCounts struct {
	mu sync.Mutex
	n  map[string]int
}

func (c *httpCounts) get(key string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n[key]
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// startHTTPSyncServer serves HTTPSyncHandler at /sync/ and returns the URL
// of document "doc".
func startHTTPSyncServer(t *testing.T, auth func(r *http.Request, name string, write bool) error) (string, *httpCounts) {
	t.Helper()
	if auth == nil {
		auth = func(*http.Request, string, bool) error { return nil }
	}
	s := server.New(server.Options{OnHTTPRequest: auth})
	counts := &httpCounts{n: map[string]int{}}
	mux := http.NewServeMux()
	h := s.HTTPSyncHandler()
	mux.Handle("/sync/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		h.ServeHTTP(rec, r)
		counts.mu.Lock()
		counts.n[fmt.Sprintf("%s %d", r.Method, rec.status)]++
		counts.n[r.Method]++
		counts.mu.Unlock()
	}))
	hs := httptest.NewServer(mux)
	t.Cleanup(func() {
		hs.Close()
		_ = s.Close(context.Background())
	})
	return hs.URL + "/sync/doc", counts
}

func items(d *doc.Doc) *types.Array { return types.NewArray(d.Branch("items")) }

func push(d *doc.Doc, v string) {
	a := items(d)
	txn := d.WriteTxn()
	a.Push(txn, v)
	txn.Commit()
}

func mustSync(t *testing.T, h *client.HTTPSync, d *doc.Doc) bool {
	t.Helper()
	changed, err := h.Sync(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	return changed
}

// TestHTTPSync_Converges: two documents converge through the server, a
// Sync with nothing new costs one 304 and no POST, and a local delete is
// pushed though it adds no struct.
func TestHTTPSync_Converges(t *testing.T) {
	u, counts := startHTTPSyncServer(t, nil)
	a, b := doc.NewDocWithOptions(doc.Options{ClientID: 1}), doc.NewDocWithOptions(doc.Options{ClientID: 2})
	sa, sb := &client.HTTPSync{URL: u}, &client.HTTPSync{URL: u, V2: true}
	defer sa.Close()
	defer sb.Close()
	push(a, "a1")
	mustSync(t, sa, a)
	if !mustSync(t, sb, b) {
		t.Fatal("b's first sync changed nothing")
	}
	push(b, "b1")
	mustSync(t, sb, b)
	mustSync(t, sa, a)
	if ga, gb := fmt.Sprint(items(a).ToSlice()), fmt.Sprint(items(b).ToSlice()); ga != gb || len(items(a).ToSlice()) != 2 {
		t.Fatalf("a %s, b %s", ga, gb)
	}
	posts := counts.get("POST")
	if mustSync(t, sa, a) {
		t.Fatal("an idle sync changed the document")
	}
	if counts.get("POST") != posts || counts.get("GET 304") == 0 {
		t.Fatalf("idle sync: POSTs %d -> %d, 304s %d", posts, counts.get("POST"), counts.get("GET 304"))
	}
	// A delete adds no struct; it is pushed anyway.
	arr := items(a)
	txn := a.WriteTxn()
	arr.Delete(txn, 0, 1)
	txn.Commit()
	mustSync(t, sa, a)
	if counts.get("POST") != posts+1 {
		t.Fatalf("the delete was not pushed (%d POSTs)", counts.get("POST")-posts)
	}
	// What b pulls is not pushed back.
	posts = counts.get("POST")
	mustSync(t, sb, b)
	if got := items(b).ToSlice(); len(got) != 1 {
		t.Fatalf("b after the delete: %v", got)
	}
	mustSync(t, sb, b)
	if counts.get("POST") != posts {
		t.Fatalf("b pushed what it pulled (%d POSTs)", counts.get("POST")-posts)
	}
}

// TestHTTPSync_FreshDocument: an HTTPSync used with another document starts
// afresh (no stale ETag, the whole document pulled).
func TestHTTPSync_FreshDocument(t *testing.T) {
	u, _ := startHTTPSyncServer(t, nil)
	a := doc.NewDoc()
	push(a, "x")
	s := &client.HTTPSync{URL: u}
	defer s.Close()
	mustSync(t, s, a)
	mustSync(t, s, a)
	fresh := doc.NewDoc()
	if !mustSync(t, s, fresh) || len(items(fresh).ToSlice()) != 1 {
		t.Fatalf("fresh document got %v", items(fresh).ToSlice())
	}
}

// TestHTTPSync_EmptyDocumentPushesNothing: the first Sync of an empty
// document is one GET.
func TestHTTPSync_EmptyDocumentPushesNothing(t *testing.T) {
	u, counts := startHTTPSyncServer(t, nil)
	s := &client.HTTPSync{URL: u}
	defer s.Close()
	mustSync(t, s, doc.NewDoc())
	if counts.get("POST") != 0 || counts.get("GET") != 1 {
		t.Fatalf("POSTs %d, GETs %d", counts.get("POST"), counts.get("GET"))
	}
}

// TestHTTPSync_ErrorKeepsLocalChanges: a refused push is retried by the
// next Sync, also a delete, which adds no struct.
func TestHTTPSync_ErrorKeepsLocalChanges(t *testing.T) {
	var refuse atomic.Bool
	u, _ := startHTTPSyncServer(t, func(r *http.Request, name string, write bool) error {
		if write && refuse.Load() {
			return &server.HTTPError{Status: http.StatusUnauthorized, Message: "token expired"}
		}
		return nil
	})
	a := doc.NewDoc()
	push(a, "one")
	push(a, "two")
	s := &client.HTTPSync{URL: u}
	defer s.Close()
	mustSync(t, s, a)
	refuse.Store(true)
	arr := items(a)
	txn := a.WriteTxn()
	arr.Delete(txn, 0, 1)
	txn.Commit()
	if _, err := s.Sync(context.Background(), a); err == nil {
		t.Fatal("a refused push returned no error")
	}
	refuse.Store(false)
	mustSync(t, s, a)
	b := doc.NewDoc()
	mustSync(t, &client.HTTPSync{URL: u}, b)
	if got := items(b).ToSlice(); fmt.Sprint(got) != "[two]" {
		t.Fatalf("the delete was lost: %v", got)
	}
}
