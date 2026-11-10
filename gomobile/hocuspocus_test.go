package gomobile

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	syncpkg "github.com/Deln0r/ygo/internal/sync"
)

// authOnlyServer answers a Hocuspocus Auth frame: PermissionDenied
// "bad-token" for the token "bad", Authenticated read-write otherwise.
func authOnlyServer(t *testing.T) string {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		for {
			_, raw, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			key, rest, err := syncpkg.DecodeRoute(raw)
			if err != nil {
				return
			}
			f, _, err := syncpkg.DecodeEnvelope(rest)
			if err != nil || f.Type != syncpkg.MessageAuth {
				continue
			}
			reply := syncpkg.EncodeAuthAuthenticated(syncpkg.ScopeReadWrite)
			if string(f.Payload) == "bad" {
				reply = syncpkg.EncodeAuthPermissionDenied("bad-token")
			}
			_ = conn.Write(r.Context(), websocket.MessageBinary, append(syncpkg.EncodeRoute(key), reply...))
		}
	}))
	t.Cleanup(ts.Close)
	return "ws://" + strings.TrimPrefix(ts.URL, "http://")
}

type authRecorder struct {
	mu     sync.Mutex
	scopes []string
	denied []string
}

func (a *authRecorder) OnAuthenticated(scope string) {
	a.mu.Lock()
	a.scopes = append(a.scopes, scope)
	a.mu.Unlock()
}

func (a *authRecorder) OnPermissionDenied(reason string) {
	a.mu.Lock()
	a.denied = append(a.denied, reason)
	a.mu.Unlock()
}

func (a *authRecorder) wait(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		a.mu.Lock()
		ok := cond()
		a.mu.Unlock()
		if ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

// The AuthListener gets the scope for an accepted token and the server's
// reason for a refused one, which a native app needs without parsing an
// error string; Scope reports the grant.
func TestHocuspocus_AuthListener(t *testing.T) {
	url := authOnlyServer(t)

	ok := NewClient(url, "doc", NewDoc())
	okRec := &authRecorder{}
	ok.SetHocuspocus("ok")
	ok.SetAuthListener(okRec)
	if err := ok.Connect(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ok.Close() })
	okRec.wait(t, "OnAuthenticated", func() bool { return len(okRec.scopes) > 0 && okRec.scopes[0] == "read-write" })
	if ok.Scope() != "read-write" {
		t.Errorf("Scope %q", ok.Scope())
	}

	bad := NewClient(url, "doc", NewDoc())
	badRec := &authRecorder{}
	bad.SetHocuspocus("bad")
	bad.SetAuthListener(badRec)
	if err := bad.Connect(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bad.Close() })
	badRec.wait(t, "OnPermissionDenied", func() bool { return len(badRec.denied) > 0 && badRec.denied[0] == "bad-token" })
}
