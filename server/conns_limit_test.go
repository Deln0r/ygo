package server_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Deln0r/ygo/server"
)

// TestServer_MaxConnsPerDoc_RejectsOverCap exercises the per-document
// connection cap end to end: connections up to the limit are admitted,
// the next is refused at the upgrade with websocket.StatusPolicyViolation
// and no initial sync, and a slot freed by a departing client is reusable
// (the cap is a live count, not a high-water mark).
func TestServer_MaxConnsPerDoc_RejectsOverCap(t *testing.T) {
	wsURL, _ := startTestServer(t, server.Options{
		OriginPatterns: []string{"*"},
		MaxConnsPerDoc: 2,
	})

	const docName = "capped"

	// Two clients fill the room. Reading the initial SyncStep1 proves
	// each is registered: addConn runs before SendInitialSync, so a
	// received sync frame means the slot is already taken.
	a := dialClient(t, wsURL, docName, 1)
	defer a.close()
	_ = a.read(t)
	b := dialClient(t, wsURL, docName, 2)
	defer b.close()
	_ = b.read(t)

	// Third client: the WS upgrade succeeds, then the server refuses it
	// with a policy-violation close before any sync.
	if got := dialExpectClose(t, wsURL, docName); got != websocket.StatusPolicyViolation {
		t.Fatalf("over-cap close status = %d, want %d (PolicyViolation)",
			got, websocket.StatusPolicyViolation)
	}

	// Free a slot; a fresh client must now be admitted once the server
	// has processed b's departure.
	b.close()
	expectAdmittedEventually(t, wsURL, docName)
}

// dialExpectClose dials a client the server is expected to refuse and
// returns the WebSocket close status it observes. A refused connection
// receives no initial sync, so the first Read sees the close.
func dialExpectClose(t *testing.T, wsURL, docName string) websocket.StatusCode {
	t.Helper()
	dialURL := strings.TrimRight(wsURL, "/") + "/" + docName
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, dialURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "test done")
	for i := 0; i < 4; i++ { // drain any stray frame, then expect the close
		if _, _, rerr := c.Read(ctx); rerr != nil {
			return websocket.CloseStatus(rerr)
		}
	}
	t.Fatal("refused connection never closed")
	return -1
}

// expectAdmittedEventually retries dialing until a connection is
// admitted (reads its initial sync) or the deadline passes. The retry
// covers the asynchronous gap between a client closing and the server's
// releaseConn dropping it from the room.
func expectAdmittedEventually(t *testing.T, wsURL, docName string) {
	t.Helper()
	dialURL := strings.TrimRight(wsURL, "/") + "/" + docName
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		c, _, err := websocket.Dial(ctx, dialURL, nil)
		if err != nil {
			cancel()
			time.Sleep(20 * time.Millisecond)
			continue
		}
		_, _, rerr := c.Read(ctx)
		_ = c.Close(websocket.StatusNormalClosure, "test done")
		cancel()
		if rerr == nil {
			return // admitted: a slot was reused
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("never admitted after a slot was freed")
}
