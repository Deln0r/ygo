package server_test

import (
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Deln0r/ygo/server"
)

// TestServer_OnConnectOnDisconnect exercises the connection lifecycle
// hooks end to end: OnConnect can reject a connection by docName (it
// closes with StatusPolicyViolation and fires NO OnDisconnect), an
// accepted connection proceeds to sync, and its later close fires
// exactly one OnDisconnect carrying the same connID OnConnect saw.
func TestServer_OnConnectOnDisconnect(t *testing.T) {
	var mu sync.Mutex
	var connected []string    // connIDs OnConnect accepted
	var disconnected []string // connIDs OnDisconnect reported

	wsURL, _ := startTestServer(t, server.Options{
		OriginPatterns: []string{"*"},
		OnConnect: func(connID, docName string, r *http.Request) error {
			if r == nil {
				return errors.New("nil request")
			}
			if docName == "blocked" {
				return errors.New("denied by policy")
			}
			mu.Lock()
			connected = append(connected, connID)
			mu.Unlock()
			return nil
		},
		OnDisconnect: func(connID, docName string) {
			mu.Lock()
			disconnected = append(disconnected, connID)
			mu.Unlock()
		},
	})

	// Rejected connection: OnConnect returns an error, so the server
	// closes with a policy-violation and never records OnConnect.
	if got := dialExpectClose(t, wsURL, "blocked"); got != websocket.StatusPolicyViolation {
		t.Fatalf("rejected close status = %d, want %d (PolicyViolation)",
			got, websocket.StatusPolicyViolation)
	}

	// Accepted connection: connects, reads the initial sync (which is
	// only sent after OnConnect passed), then closes.
	a := dialClient(t, wsURL, "allowed", 1)
	a.read(t)
	a.close()

	// OnDisconnect fires asynchronously on the server's serve goroutine.
	waitFor(t, 3*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(disconnected) == 1
	})

	mu.Lock()
	defer mu.Unlock()
	if len(connected) != 1 {
		t.Fatalf("OnConnect accepted = %d, want 1 (a rejected conn must not count)", len(connected))
	}
	if len(disconnected) != 1 {
		t.Fatalf("OnDisconnect fired = %d, want 1 (a rejected conn must not fire it)", len(disconnected))
	}
	if connected[0] != disconnected[0] {
		t.Fatalf("connID mismatch: OnConnect %q vs OnDisconnect %q", connected[0], disconnected[0])
	}
}

// TestServer_OnConnect_PanicDoesNotLeakSlot guards the panic-safety of
// the OnConnect call site: if the adopter's hook panics, the admitted
// connection must still be released so the room's slot is not leaked.
// With a per-doc cap of 1 a leaked slot would permanently refuse the
// room; instead a fresh connection must be admitted once the panicked
// one is torn down.
func TestServer_OnConnect_PanicDoesNotLeakSlot(t *testing.T) {
	var panicked atomic.Bool
	wsURL, _ := startTestServer(t, server.Options{
		OriginPatterns: []string{"*"},
		MaxConnsPerDoc: 1,
		OnConnect: func(connID, docName string, r *http.Request) error {
			if panicked.CompareAndSwap(false, true) {
				panic("boom in OnConnect")
			}
			return nil
		},
	})

	// First dial: OnConnect panics. net/http recovers on the serve
	// goroutine, but the cleanup defer must have released the slot.
	d := dialClient(t, wsURL, "room", 1)
	d.close()

	// The slot must be free: a fresh connection to the same cap-1 room is
	// admitted. A leaked slot would refuse it with PolicyViolation.
	expectAdmittedEventually(t, wsURL, "room")
}

// waitFor polls cond until it holds or the timeout elapses.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}
