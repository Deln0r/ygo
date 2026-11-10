package server_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Deln0r/ygo/internal/encoding"
	syncpkg "github.com/Deln0r/ygo/internal/sync"
	"github.com/Deln0r/ygo/internal/types"
	"github.com/Deln0r/ygo/server"
)

// defaultReadLimit mirrors the server's default when Options.ReadLimit is
// zero.
const defaultReadLimit = 4 << 20

// largeUpdate stores one value on c and returns a full-state update whose
// encoding exceeds over bytes, plus the value. It fails the test if the
// update is not actually over, so the test cannot silently stop
// exercising the limit.
func largeUpdate(t *testing.T, c *wsClient, key string, over int) (update []byte, value string) {
	t.Helper()
	value = strings.Repeat("x", over+1000)
	m := types.NewMap(c.doc.Branch("doc"))
	txn := c.doc.WriteTxn()
	m.Set(txn, key, value)
	txn.Commit()
	update = encoding.EncodeStateAsUpdate(c.doc)
	if len(update) <= over {
		t.Fatalf("setup: update is %d bytes, want > %d to exercise the read limit", len(update), over)
	}
	return update, value
}

// syncLarge sends a value whose update exceeds over bytes from one client
// and waits for the other to receive it.
func syncLarge(t *testing.T, opts server.Options, over int) {
	t.Helper()
	wsURL, _ := startTestServer(t, opts)

	a := dialClient(t, wsURL, "big", 100)
	defer a.close()
	a.conn.SetReadLimit(-1) // receive the large broadcast back
	a.read(t)               // initial SyncStep1

	b := dialClient(t, wsURL, "big", 200)
	defer b.close()
	b.conn.SetReadLimit(-1) // the real ygo client lifts its own limit too
	b.read(t)               // initial SyncStep1

	update, value := largeUpdate(t, a, "blob", over)
	a.write(t, syncpkg.EncodeSyncUpdate(update))

	b.readUntil(t, func(f *syncpkg.Frame) bool {
		if f.Type != syncpkg.MessageSync || f.SyncSub != syncpkg.SyncUpdate {
			return false
		}
		if err := encoding.ApplyUpdate(b.doc, f.Payload); err != nil {
			t.Fatalf("apply update: %v", err)
		}
		got, _ := types.NewMap(b.doc.Branch("doc")).Get("blob").(string)
		return got == value
	})
}

// TestServer_ReadLimit_DefaultTakesAPaste: with ReadLimit unset, a 50 000
// character paste reaches the peer. Up to v1.31 the default was 32 KiB,
// the server closed the sender with 1009, and the y-websocket client
// resent the same message on every reconnect.
func TestServer_ReadLimit_DefaultTakesAPaste(t *testing.T) {
	syncLarge(t, server.Options{OriginPatterns: []string{"*"}}, 50000)
}

// TestServer_ReadLimit_AllowsLargeDoc: with ReadLimit raised above the
// default, a client syncs a document larger than the default and a peer
// receives it. Without the option the server would close the sender on
// read and the peer would never see the update.
func TestServer_ReadLimit_AllowsLargeDoc(t *testing.T) {
	syncLarge(t, server.Options{
		OriginPatterns: []string{"*"},
		ReadLimit:      8 << 20, // 8 MiB
	}, defaultReadLimit)
}

// TestServer_ReadLimit_DefaultDropsOversizeFrame: at the default limit
// (ReadLimit unset), a frame larger than 4 MiB makes the server close the
// sending connection with 1009, the failure mode the option exists to
// avoid.
func TestServer_ReadLimit_DefaultDropsOversizeFrame(t *testing.T) {
	wsURL, _ := startTestServer(t, server.Options{OriginPatterns: []string{"*"}})

	a := dialClient(t, wsURL, "big", 100)
	defer a.close()
	a.read(t) // initial SyncStep1

	// The server sends its close frame and cuts the connection without
	// draining the oversize payload, so this write can fail while the
	// frame is still going out. The read below gets the close either way.
	update, _ := largeUpdate(t, a, "blob", defaultReadLimit)
	_ = a.conn.Write(a.ctx, websocket.MessageBinary, syncpkg.EncodeSyncUpdate(update))

	ctx, cancel := context.WithTimeout(a.ctx, 3*time.Second)
	defer cancel()
	_, _, err := a.conn.Read(ctx)
	if got := websocket.CloseStatus(err); got != websocket.StatusMessageTooBig {
		t.Fatalf("read after an oversize frame: %v (close status %d), want 1009", err, got)
	}
}
