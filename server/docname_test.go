package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Deln0r/ygo/internal/encoding"
	syncpkg "github.com/Deln0r/ygo/internal/sync"
	"github.com/Deln0r/ygo/internal/types"
	"github.com/Deln0r/ygo/server"
)

// changedDocs records the docName of every applied update.
type changedDocs struct {
	mu    sync.Mutex
	names []string
	seen  chan struct{}
}

func newChangedDocs() *changedDocs {
	return &changedDocs{seen: make(chan struct{}, 16)}
}

func (c *changedDocs) onChange(docName string, _ []byte) {
	c.mu.Lock()
	c.names = append(c.names, docName)
	c.mu.Unlock()
	c.seen <- struct{}{}
}

func (c *changedDocs) wait(t *testing.T, n int) []string {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for i := 0; i < n; i++ {
		select {
		case <-c.seen:
		case <-deadline:
			t.Fatalf("timed out after %d of %d updates", i, n)
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.names...)
}

// sendEdit writes one map entry from c as a sync update.
func sendEdit(t *testing.T, c *wsClient, value string) {
	t.Helper()
	m := types.NewMap(c.doc.Branch("m"))
	txn := c.doc.WriteTxn()
	m.Set(txn, "k", value)
	txn.Commit()
	c.write(t, syncpkg.EncodeSyncUpdate(encoding.EncodeStateAsUpdate(c.doc)))
}

// TestServer_RoomsWithSlashStayApart: two rooms that differ only in their
// first path segment are two documents, whether the handler is used
// directly or mounted under a ServeMux prefix. Before v1.32.0 both became
// the document "doc", so each room saw the other's edits.
func TestServer_RoomsWithSlashStayApart(t *testing.T) {
	for _, tc := range []struct {
		name, mount, prefix string
	}{
		{"handler used directly", "", ""},
		{"mounted at /collab/", "/collab/", "/collab"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := newChangedDocs()
			srv := server.New(server.Options{OriginPatterns: []string{"*"}, OnChange: changed.onChange})
			h := srv.Handler()
			if tc.mount != "" {
				mux := http.NewServeMux()
				mux.Handle(tc.mount, srv.Handler())
				h = mux
			}
			ts := httptest.NewServer(h)
			t.Cleanup(func() {
				ts.Close()
				_ = srv.Close(context.Background())
			})
			base := "ws://" + strings.TrimPrefix(ts.URL, "http://") + tc.prefix

			a := dialClient(t, base, "team-a/doc", 100)
			defer a.close()
			a.read(t)
			b := dialClient(t, base, "team-b/doc", 200)
			defer b.close()
			b.read(t)

			sendEdit(t, a, "secret of team a")
			sendEdit(t, b, "team b")
			got := changed.wait(t, 2)
			if strings.Join(got, ",") != "team-a/doc,team-b/doc" && strings.Join(got, ",") != "team-b/doc,team-a/doc" {
				t.Fatalf("updates went to documents %q, want team-a/doc and team-b/doc", got)
			}
			if st := srv.Stats(); st.Documents != 2 {
				t.Fatalf("resident documents = %d, want 2", st.Documents)
			}
		})
	}
}

// TestServer_MountPrefixIsNotPartOfTheName: under a ServeMux prefix a
// single-segment room keeps the name it had before v1.32.0.
func TestServer_MountPrefixIsNotPartOfTheName(t *testing.T) {
	changed := newChangedDocs()
	srv := server.New(server.Options{OriginPatterns: []string{"*"}, OnChange: changed.onChange})
	mux := http.NewServeMux()
	mux.Handle("/collab/", srv.Handler())
	ts := httptest.NewServer(mux)
	t.Cleanup(func() {
		ts.Close()
		_ = srv.Close(context.Background())
	})
	a := dialClient(t, "ws://"+strings.TrimPrefix(ts.URL, "http://")+"/collab", "room1", 100)
	defer a.close()
	a.read(t)
	sendEdit(t, a, "x")
	if got := changed.wait(t, 1); len(got) != 1 || got[0] != "room1" {
		t.Fatalf("update went to %q, want room1", got)
	}
}
