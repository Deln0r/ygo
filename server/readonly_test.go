package server_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Deln0r/ygo/internal/encoding"
	syncpkg "github.com/Deln0r/ygo/internal/sync"
	"github.com/Deln0r/ygo/internal/types"
	"github.com/Deln0r/ygo/server"
)

// TestServer_ReadOnly_ViewerCannotWriteButReceives wires Options.ReadOnly
// end to end: a viewer marked read-only (here via a ?ro=1 query) still
// receives an editor's update, but its own edit is dropped by the server
// and never reaches the shared document, proven by a fresh observer that
// pulls the server state and sees only the editor's item.
func TestServer_ReadOnly_ViewerCannotWriteButReceives(t *testing.T) {
	wsURL, _ := startTestServer(t, server.Options{
		OriginPatterns: []string{"*"},
		ReadOnly: func(docName string, r *http.Request) bool {
			return r.URL.Query().Get("ro") == "1"
		},
	})

	editor := dialClient(t, wsURL, "room", 1)
	defer editor.close()
	editor.read(t) // initial SyncStep1
	viewer := dialClient(t, wsURL, "room?ro=1", 2)
	defer viewer.close()
	viewer.read(t)

	// The editor edits; the read-only viewer must still RECEIVE it.
	pushItem(t, editor, "items", "from-editor")
	viewerArr := types.NewArray(viewer.doc.Branch("items"))
	applyUntil(t, viewer, viewerArr, 1)
	if viewerArr.Len() != 1 {
		t.Fatalf("read-only viewer did not receive editor edit: len=%d", viewerArr.Len())
	}

	// The viewer edits; the server must DROP it. Sending the viewer's own
	// SyncStep1 after the edit forces the server to process the edit frame
	// first (frames are read in order), so the SyncStep2 reply proves the
	// edit was seen (and dropped) server-side.
	pushItem(t, viewer, "items", "from-viewer")
	viewer.write(t, syncpkg.EncodeSyncStep1(encoding.EncodeStateVector(map[uint64]uint64{}, nil)))
	viewer.readUntil(t, func(f *syncpkg.Frame) bool {
		return f.Type == syncpkg.MessageSync && f.SyncSub == syncpkg.SyncStep2
	})

	// A fresh observer pulls the server state: it must hold only the
	// editor's item. If the read-only edit had leaked, it would see two.
	observer := dialClient(t, wsURL, "room", 3)
	defer observer.close()
	observer.read(t)
	observer.write(t, syncpkg.EncodeSyncStep1(encoding.EncodeStateVector(map[uint64]uint64{}, nil)))
	obsArr := types.NewArray(observer.doc.Branch("items"))
	applyUntil(t, observer, obsArr, 1)
	if obsArr.Len() != 1 {
		t.Fatalf("server holds %d items, want 1 (read-only edit must be dropped)", obsArr.Len())
	}
}

// pushItem appends value to the named array on the client's local doc and
// sends the resulting state as a SyncUpdate.
func pushItem(t *testing.T, c *wsClient, branch, value string) {
	t.Helper()
	arr := types.NewArray(c.doc.Branch(branch))
	txn := c.doc.WriteTxn()
	arr.Push(txn, value)
	txn.Commit()
	c.write(t, syncpkg.EncodeSyncUpdate(encoding.EncodeStateAsUpdate(c.doc)))
}

// applyUntil drains frames, applying inbound sync content to the client's
// doc until arr reaches n elements or the deadline passes.
func applyUntil(t *testing.T, c *wsClient, arr *types.Array, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for arr.Len() < uint64(n) && time.Now().Before(deadline) {
		readCtx, cancel := context.WithTimeout(c.ctx, 300*time.Millisecond)
		_, raw, err := c.conn.Read(readCtx)
		cancel()
		if err != nil {
			continue
		}
		f, _, derr := syncpkg.DecodeEnvelope(raw)
		if derr != nil {
			continue
		}
		if f.Type == syncpkg.MessageSync &&
			(f.SyncSub == syncpkg.SyncStep2 || f.SyncSub == syncpkg.SyncUpdate) {
			_ = encoding.ApplyUpdate(c.doc, f.Payload)
		}
	}
}
