package server_test

import (
	"fmt"
	"sync"
	"testing"

	"github.com/Deln0r/ygo/internal/encoding"
	syncpkg "github.com/Deln0r/ygo/internal/sync"
	"github.com/Deln0r/ygo/internal/types"
	"github.com/Deln0r/ygo/server"
)

// TestServer_ConnectDisconnectChurn_NoRace hammers a single document
// with concurrent connect/edit/disconnect cycles. With no long-lived
// connection the room repeatedly drains to zero and is re-acquired,
// exercising exactly the admitConn/releaseConn interplay the split-room
// fix touches.
//
// The split-room race is timing-narrow and not deterministically
// reproducible, so this is a -race / deadlock stress: its value is the
// race detector plus the post-churn health check (a fresh client must
// still connect, sync, and see its own edit). It deliberately uses no
// store — a store would only add the sqlite backend's own concurrency
// limits as noise. The deterministic guard for the fix itself is the
// admitConn registration invariant in TestAdmitConn_RegistersAtomically.
func TestServer_ConnectDisconnectChurn_NoRace(t *testing.T) {
	wsURL, _ := startTestServer(t, server.Options{OriginPatterns: []string{"*"}})

	const writers = 6
	const perWriter = 5
	const docName = "churn"
	emptySV := encoding.EncodeStateVector(map[uint64]uint64{}, nil)

	var wg sync.WaitGroup
	for g := 0; g < writers; g++ {
		g := g
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				// Unique clientID per live connection: concurrent
				// connections must never share a CRDT clientID.
				clientID := uint64(1000 + g*perWriter + i)
				c := dialClient(t, wsURL, docName, clientID)
				c.read(t) // server's initial SyncStep1

				arr := types.NewArray(c.doc.Branch("items"))
				txn := c.doc.WriteTxn()
				arr.Push(txn, fmt.Sprintf("g%d-i%d", g, i))
				txn.Commit()
				c.write(t, syncpkg.EncodeSyncUpdate(encoding.EncodeStateAsUpdate(c.doc)))

				// Round-trip through a SyncStep1 pull so the server has
				// processed this connection before we drop it (frames are
				// read in order), keeping the churn busy rather than racing
				// straight to close.
				c.write(t, syncpkg.EncodeSyncStep1(emptySV))
				c.readUntil(t, func(f *syncpkg.Frame) bool {
					return f.Type == syncpkg.MessageSync && f.SyncSub == syncpkg.SyncStep2
				})
				c.close()
			}
		}()
	}
	wg.Wait()

	// The server must still be healthy after the churn: a fresh client
	// connects, completes the handshake, contributes, and sees its own
	// edit echoed. A deadlock or a registry corrupted by the admit/evict
	// interplay would hang or fail here.
	c := dialClient(t, wsURL, docName, 9999)
	defer c.close()
	c.read(t)
	arr := types.NewArray(c.doc.Branch("items"))
	txn := c.doc.WriteTxn()
	arr.Push(txn, "final")
	txn.Commit()
	c.write(t, syncpkg.EncodeSyncUpdate(encoding.EncodeStateAsUpdate(c.doc)))
	c.readUntil(t, func(f *syncpkg.Frame) bool {
		return f.Type == syncpkg.MessageSync && f.SyncSub == syncpkg.SyncUpdate
	})
}
