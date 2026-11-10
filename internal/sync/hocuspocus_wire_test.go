package sync_test

import (
	"encoding/hex"
	"testing"

	syncpkg "github.com/Deln0r/ygo/internal/sync"
)

// Bytes captured from @hocuspocus/provider 2.15.3, 3.4.4, 4.8.0 and
// @hocuspocus/server 4.8.0 on 10 Nov 2026 (docs: HOCUSPOCUS_PROTOCOL.md),
// with the routing key removed where the test is about the envelope.
func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestHocusWire_Captured: ygo's encoders write what Hocuspocus writes, and
// its decoder reads what providers send, including the parts the old code
// rejected (a Token with a version after it, a server token request with
// no fields, a provider CLOSE with no reason).
func TestHocusWire_Captured(t *testing.T) {
	for _, c := range []struct {
		name string
		got  []byte
		want string
	}{
		{"Authenticated read-write", syncpkg.EncodeAuthAuthenticated(syncpkg.ScopeReadWrite), "02020a726561642d7772697465"},
		{"Authenticated readonly", syncpkg.EncodeAuthAuthenticated(syncpkg.ScopeReadOnly), "020208726561646f6e6c79"},
		{"PermissionDenied", syncpkg.EncodeAuthPermissionDenied("permission-denied"), "0201117065726d697373696f6e2d64656e696564"},
		{"server CLOSE", syncpkg.EncodeClose("provider_initiated"), "071270726f76696465725f696e69746961746564"},
		{"SyncStatus 1", syncpkg.EncodeSyncStatus(true), "0801"},
		{"SyncStatus 0", syncpkg.EncodeSyncStatus(false), "0800"},
		{"Token from a client", syncpkg.EncodeAuthToken("ok"), "0200026f6b"},
		{"routing key", syncpkg.EncodeRoute("seeded-b"), "087365656465642d62"},
	} {
		if h := hex.EncodeToString(c.got); h != c.want {
			t.Errorf("%s: %s, want %s", c.name, h, c.want)
		}
	}

	for _, c := range []struct {
		name     string
		wire     string
		typ      syncpkg.MessageType
		sub      syncpkg.AuthSubType
		payload  string
		tailWant string
	}{
		{"provider 4 Token with version", "0200026f6b05342e382e30", syncpkg.MessageAuth, syncpkg.AuthToken, "ok", "05342e382e30"},
		{"provider 3 Token, no token", "020000", syncpkg.MessageAuth, syncpkg.AuthToken, "", ""},
		{"server token request", "0200", syncpkg.MessageAuth, syncpkg.AuthToken, "", ""},
		{"Authenticated", "02020a726561642d7772697465", syncpkg.MessageAuth, syncpkg.AuthAuthenticated, "read-write", ""},
		{"PermissionDenied", "0201117065726d697373696f6e2d64656e696564", syncpkg.MessageAuth, syncpkg.AuthPermissionDenied, "permission-denied", ""},
		{"provider CLOSE", "07", syncpkg.MessageClose, 0, "", ""},
		{"server CLOSE", "071270726f76696465725f696e69746961746564", syncpkg.MessageClose, 0, "provider_initiated", ""},
	} {
		f, tail, err := syncpkg.DecodeEnvelope(mustHex(t, c.wire))
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if f.Type != c.typ || (c.typ == syncpkg.MessageAuth && f.AuthSub != c.sub) || string(f.Payload) != c.payload || hex.EncodeToString(tail) != c.tailWant {
			t.Errorf("%s: type %d sub %d payload %q tail %x", c.name, f.Type, f.AuthSub, f.Payload, tail)
		}
	}

	key, rest, err := syncpkg.DecodeRoute(mustHex(t, "11646f632d73007662386a663466336a3372020002"))
	if err != nil || key != "doc-s\x00vb8jf4f3j3r" || syncpkg.RouteDocName(key) != "doc-s" || hex.EncodeToString(rest) != "020002" {
		t.Errorf("session key: %q %x %v, name %q", key, rest, err, syncpkg.RouteDocName(key))
	}
}
