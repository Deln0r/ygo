package sync_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/Deln0r/ygo/internal/awareness"
	"github.com/Deln0r/ygo/internal/doc"
	syncpkg "github.com/Deln0r/ygo/internal/sync"
)

// hocusTransport extends memTransport with the AuthFailed flag
// observability the server transport relies on.
type hocusTransport struct {
	memTransport
}

func newHocusConn(t *testing.T, docName string) (*syncpkg.Conn, *hocusTransport) {
	t.Helper()
	tr := &hocusTransport{}
	c := syncpkg.New(doc.NewDoc(), awareness.New(1), "conn-1")
	c.DocName = docName
	c.Send = tr.send
	c.Broadcast = tr.broadcast
	return c, tr
}

func TestHocus_AuthToken_NoCallback_AcceptsSilently(t *testing.T) {
	conn, tr := newHocusConn(t, "doc-A")

	wire := syncpkg.EncodeAuthToken("any-token")
	frame, _, _ := syncpkg.DecodeEnvelope(wire)
	if err := conn.HandleFrame(frame); err != nil {
		t.Fatal(err)
	}
	if conn.AuthFailed {
		t.Error("AuthFailed set without OnAuthenticate")
	}

	// Server replies with Authenticated so Hocuspocus clients flip
	// the internal flag.
	if len(tr.Sent) != 1 {
		t.Fatalf("Sent count = %d, want 1", len(tr.Sent))
	}
	reply, _, _ := syncpkg.DecodeEnvelope(tr.Sent[0])
	if reply.Type != syncpkg.MessageAuth || reply.AuthSub != syncpkg.AuthAuthenticated {
		t.Errorf("reply = %d/%d, want Auth/Authenticated", reply.Type, reply.AuthSub)
	}
}

func TestHocus_AuthToken_CallbackAccepts(t *testing.T) {
	conn, tr := newHocusConn(t, "doc-A")
	var seenDocName, seenToken string
	conn.OnAuthenticate = func(docName, token string) error {
		seenDocName = docName
		seenToken = token
		return nil
	}

	wire := syncpkg.EncodeAuthToken("good-token")
	frame, _, _ := syncpkg.DecodeEnvelope(wire)
	if err := conn.HandleFrame(frame); err != nil {
		t.Fatal(err)
	}
	if seenDocName != "doc-A" || seenToken != "good-token" {
		t.Errorf("callback args = (%q, %q), want (doc-A, good-token)", seenDocName, seenToken)
	}
	if conn.AuthFailed {
		t.Error("AuthFailed set on accept")
	}
	if len(tr.Sent) != 1 {
		t.Fatalf("Sent count = %d, want 1", len(tr.Sent))
	}
}

func TestHocus_AuthToken_CallbackDenies(t *testing.T) {
	conn, tr := newHocusConn(t, "doc-A")
	conn.OnAuthenticate = func(_, _ string) error {
		return errors.New("token expired")
	}

	wire := syncpkg.EncodeAuthToken("bad-token")
	frame, _, _ := syncpkg.DecodeEnvelope(wire)
	if err := conn.HandleFrame(frame); err != nil {
		t.Fatal(err)
	}
	if !conn.AuthFailed {
		t.Error("AuthFailed not set on deny")
	}
	if len(tr.Sent) != 2 {
		t.Fatalf("Sent count = %d, want 2 (PermissionDenied + Close)", len(tr.Sent))
	}
	first, _, _ := syncpkg.DecodeEnvelope(tr.Sent[0])
	if first.Type != syncpkg.MessageAuth || first.AuthSub != syncpkg.AuthPermissionDenied {
		t.Errorf("first reply = %d/%d, want Auth/PermissionDenied", first.Type, first.AuthSub)
	}
	if string(first.Payload) != "token expired" {
		t.Errorf("reason = %q, want token expired", first.Payload)
	}
	second, _, _ := syncpkg.DecodeEnvelope(tr.Sent[1])
	if second.Type != syncpkg.MessageClose {
		t.Errorf("second reply = %d, want Close", second.Type)
	}
}

func TestHocus_Stateless_CallbackInvoked(t *testing.T) {
	conn, _ := newHocusConn(t, "doc-A")
	var seenDocName, seenPayload string
	conn.OnStateless = func(docName, payload string) {
		seenDocName = docName
		seenPayload = payload
	}

	wire := syncpkg.EncodeStateless(`{"rpc":"ping"}`)
	frame, _, _ := syncpkg.DecodeEnvelope(wire)
	if err := conn.HandleFrame(frame); err != nil {
		t.Fatal(err)
	}
	if seenDocName != "doc-A" || seenPayload != `{"rpc":"ping"}` {
		t.Errorf("callback args = (%q, %q)", seenDocName, seenPayload)
	}
}

func TestHocus_BroadcastStateless_FansOut(t *testing.T) {
	conn, tr := newHocusConn(t, "doc-A")

	wire := syncpkg.EncodeBroadcastStateless("hello-everyone")
	frame, _, _ := syncpkg.DecodeEnvelope(wire)
	if err := conn.HandleFrame(frame); err != nil {
		t.Fatal(err)
	}
	if len(tr.Broadcast) != 1 {
		t.Fatalf("Broadcast count = %d, want 1", len(tr.Broadcast))
	}
	echoed, _, _ := syncpkg.DecodeEnvelope(tr.Broadcast[0])
	if echoed.Type != syncpkg.MessageBroadcastStateless ||
		string(echoed.Payload) != "hello-everyone" {
		t.Errorf("broadcast = %d/%q, want BroadcastStateless/hello-everyone",
			echoed.Type, echoed.Payload)
	}
}

func TestHocus_Close_SilentlyAccepted(t *testing.T) {
	conn, tr := newHocusConn(t, "doc-A")
	wire := syncpkg.EncodeClose("client-initiated-close")
	frame, _, _ := syncpkg.DecodeEnvelope(wire)
	if err := conn.HandleFrame(frame); err != nil {
		t.Fatal(err)
	}
	if len(tr.Sent) != 0 || len(tr.Broadcast) != 0 {
		t.Errorf("Close generated traffic: Sent=%d Broadcast=%d", len(tr.Sent), len(tr.Broadcast))
	}
}

func TestHocus_SyncStatus_SilentlyAccepted(t *testing.T) {
	conn, tr := newHocusConn(t, "doc-A")
	wire := syncpkg.EncodeSyncStatus(true)
	frame, _, _ := syncpkg.DecodeEnvelope(wire)
	if err := conn.HandleFrame(frame); err != nil {
		t.Fatal(err)
	}
	if len(tr.Sent) != 0 || len(tr.Broadcast) != 0 {
		t.Errorf("SyncStatus generated traffic")
	}
	if !bytes.Equal(frame.Payload, []byte{0x01}) {
		t.Errorf("SyncStatus payload = %x, want [01]", frame.Payload)
	}
}

func TestHocus_EncodeDecode_WireBytesRoundTrip(t *testing.T) {
	// Each new encoder produces bytes that DecodeEnvelope parses
	// into a Frame with matching fields.
	cases := []struct {
		name           string
		wire           []byte
		wantType       syncpkg.MessageType
		wantAuthSub    syncpkg.AuthSubType
		wantPayloadStr string
	}{
		{"auth.token", syncpkg.EncodeAuthToken("t"), syncpkg.MessageAuth, syncpkg.AuthToken, "t"},
		{"auth.authenticated", syncpkg.EncodeAuthAuthenticated(syncpkg.ScopeReadWrite), syncpkg.MessageAuth, syncpkg.AuthAuthenticated, "read-write"},
		{"auth.permission-denied", syncpkg.EncodeAuthPermissionDenied("expired"), syncpkg.MessageAuth, syncpkg.AuthPermissionDenied, "expired"},
		{"stateless", syncpkg.EncodeStateless("p"), syncpkg.MessageStateless, 0, "p"},
		{"broadcast-stateless", syncpkg.EncodeBroadcastStateless("p"), syncpkg.MessageBroadcastStateless, 0, "p"},
		{"close", syncpkg.EncodeClose("bye"), syncpkg.MessageClose, 0, "bye"},
		{"sync-status-true", syncpkg.EncodeSyncStatus(true), syncpkg.MessageSyncStatus, 0, ""},
		{"sync-status-false", syncpkg.EncodeSyncStatus(false), syncpkg.MessageSyncStatus, 0, ""},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			frame, tail, err := syncpkg.DecodeEnvelope(c.wire)
			if err != nil {
				t.Fatal(err)
			}
			if len(tail) != 0 {
				t.Errorf("tail = %x", tail)
			}
			if frame.Type != c.wantType {
				t.Errorf("Type = %d, want %d", frame.Type, c.wantType)
			}
			if c.wantType == syncpkg.MessageAuth && frame.AuthSub != c.wantAuthSub {
				t.Errorf("AuthSub = %d, want %d", frame.AuthSub, c.wantAuthSub)
			}
			if c.wantType != syncpkg.MessageSyncStatus && string(frame.Payload) != c.wantPayloadStr {
				t.Errorf("Payload = %q, want %q", frame.Payload, c.wantPayloadStr)
			}
		})
	}
}
