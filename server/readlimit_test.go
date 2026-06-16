package server_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Deln0r/ygo/internal/encoding"
	syncpkg "github.com/Deln0r/ygo/internal/sync"
	"github.com/Deln0r/ygo/internal/types"
	"github.com/Deln0r/ygo/server"
)

// largeUpdate stores one oversized value on c and returns a full-state
// update whose encoding exceeds the 32 KiB default read limit, plus the
// value. It fails the test if the update is not actually over the limit,
// so the test cannot silently stop exercising it.
func largeUpdate(t *testing.T, c *wsClient, key string) (update []byte, value string) {
	t.Helper()
	value = strings.Repeat("x", 50000) // > 32768 once encoded
	m := types.NewMap(c.doc.Branch("doc"))
	txn := c.doc.WriteTxn()
	m.Set(txn, key, value)
	txn.Commit()
	update = encoding.EncodeStateAsUpdate(c.doc)
	if len(update) <= 32768 {
		t.Fatalf("setup: update is %d bytes, want > 32768 to exercise the read limit", len(update))
	}
	return update, value
}

// TestServer_ReadLimit_AllowsLargeDoc: with ReadLimit raised above the
// 32 KiB default, a client syncs a document larger than the default and
// a peer receives it. Without the option the server would close the
// sender on read and the peer would never see the update.
func TestServer_ReadLimit_AllowsLargeDoc(t *testing.T) {
	wsURL, _ := startTestServer(t, server.Options{
		OriginPatterns: []string{"*"},
		ReadLimit:      1 << 20, // 1 MiB
	})

	a := dialClient(t, wsURL, "big", 100)
	defer a.close()
	a.conn.SetReadLimit(1 << 20) // receive the large broadcast back
	a.read(t)                    // initial SyncStep1

	b := dialClient(t, wsURL, "big", 200)
	defer b.close()
	b.conn.SetReadLimit(1 << 20) // the real ygo client lifts its own limit too
	b.read(t)                    // initial SyncStep1

	update, value := largeUpdate(t, a, "blob")
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

// TestServer_ReadLimit_DefaultDropsOversizeFrame: at the default limit
// (ReadLimit unset), a frame larger than 32 KiB makes the server close
// the sending connection — the failure mode the option exists to avoid.
func TestServer_ReadLimit_DefaultDropsOversizeFrame(t *testing.T) {
	wsURL, _ := startTestServer(t, server.Options{OriginPatterns: []string{"*"}})

	a := dialClient(t, wsURL, "big", 100)
	defer a.close()
	a.read(t) // initial SyncStep1

	update, _ := largeUpdate(t, a, "blob")
	a.write(t, syncpkg.EncodeSyncUpdate(update))

	// The server cannot read the oversize frame and closes the conn, so
	// the next read here returns an error rather than blocking forever.
	ctx, cancel := context.WithTimeout(a.ctx, 3*time.Second)
	defer cancel()
	if _, _, err := a.conn.Read(ctx); err == nil {
		t.Fatal("expected the server to close the connection on an oversize frame at the default limit")
	}
}
