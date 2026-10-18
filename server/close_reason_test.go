package server_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/coder/websocket"

	"github.com/Deln0r/ygo/server"
)

// A load error longer than a WebSocket close reason may be (123 bytes) still
// closes the connection with 1011, its reason cut to fit; before, Close
// failed on the long reason and the client saw EOF without a close code.
func TestServer_LongLoadErrorKeepsCloseCode(t *testing.T) {
	long := strings.Repeat("é", 100) // 200 bytes of two-byte characters
	wsURL, _ := startTestServer(t, server.Options{
		OriginPatterns: []string{"*"},
		OnLoadDocument: func(string) ([]byte, error) { return nil, errors.New(long) },
	})
	if code := dialExpectClose(t, wsURL, "room"); code != websocket.StatusInternalError {
		t.Fatalf("closed with %v, want %v", code, websocket.StatusInternalError)
	}
}
