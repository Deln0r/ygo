package server_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Deln0r/ygo/internal/encoding"
	syncpkg "github.com/Deln0r/ygo/internal/sync"
	"github.com/Deln0r/ygo/internal/types"
	"github.com/Deln0r/ygo/persist/sqlite"
	"github.com/Deln0r/ygo/server"
	"github.com/Deln0r/ygo/server/backplane"
)

// A client that sends an update holding a string with invalid UTF-8 (text
// "\xff" in root "t") is refused: the server closes its connection, and the
// update reaches neither the document, nor a client already in the room, nor
// a client of another instance through the backplane, nor the store, nor
// OnChange, so it cannot keep JS clients, whose lib0 decoder rejects such
// bytes, from loading the room. Valid edits from another client go through,
// before and after.
func TestServer_RefusesInvalidUTF8Update(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "rooms.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	hub := backplane.NewMemory()
	t.Cleanup(func() { _ = hub.Close() })
	// A spy on the room's topic sees every message published to the
	// backplane, whatever the other instance does with it.
	var publishedMu sync.Mutex
	var published [][]byte
	unsub, err := hub.Conn().Subscribe("room", func(msg []byte) {
		publishedMu.Lock()
		published = append(published, append([]byte(nil), msg...))
		publishedMu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}
	defer unsub()
	var changes atomic.Int32
	wsA, _ := startTestServer(t, server.Options{
		OriginPatterns: []string{"*"},
		Store:          store,
		Backplane:      hub.Conn(),
		OnChange:       func(string, []byte) { changes.Add(1) },
	})
	wsB, _ := startTestServer(t, server.Options{OriginPatterns: []string{"*"}, Backplane: hub.Conn()})

	editor := dialClient(t, wsA, "room", 2)
	defer editor.close()
	editor.read(t)
	watcher := dialClient(t, wsA, "room", 4)
	defer watcher.close()
	watcher.read(t)
	remote := dialClient(t, wsB, "room", 5)
	defer remote.close()
	remote.read(t)

	pushItem(t, editor, "items", "before")

	bad := dialClient(t, wsA, "room", 1)
	defer bad.close()
	bad.read(t)
	raw, err := hex.DecodeString("010101000401017401ff00")
	if err != nil {
		t.Fatal(err)
	}
	bad.write(t, syncpkg.EncodeSyncUpdate(raw))
	if code := readUntilClosed(t, bad); code != websocket.StatusInternalError {
		t.Fatalf("bad client closed with %v, want %v", code, websocket.StatusInternalError)
	}

	// Anything broadcast or published for the refused update was sent before
	// the server closed the bad connection, so it reaches the watchers ahead
	// of "after".
	pushItem(t, editor, "items", "after")
	for name, c := range map[string]*wsClient{"client in the room": watcher, "client of the other instance": remote} {
		readUntilItems(t, name, c, 2)
	}

	deadline := time.Now().Add(3 * time.Second)
	for changes.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := changes.Load(); n != 2 {
		t.Fatalf("OnChange fired %d times, want 2 (the valid edits only)", n)
	}
	publishedMu.Lock()
	for i, msg := range published {
		if bytes.Contains(msg, raw) {
			t.Errorf("backplane message %d carries the refused update", i)
		}
	}
	if len(published) == 0 {
		t.Error("the spy saw no backplane message at all: the valid edits should be published")
	}
	publishedMu.Unlock()

	updates, err := store.GetUpdates(context.Background(), "room")
	if err != nil {
		t.Fatal(err)
	}
	if len(updates) != 2 {
		t.Errorf("store holds %d updates, want 2 (the valid edits only)", len(updates))
	}
	for i, u := range updates {
		if _, _, err := encoding.DecodeUpdate(u); err != nil {
			t.Errorf("stored update %d does not decode strictly: %v", i, err)
		}
	}

	observer := dialClient(t, wsA, "room", 3)
	defer observer.close()
	observer.read(t)
	observer.write(t, syncpkg.EncodeSyncStep1(encoding.EncodeStateVector(map[uint64]uint64{}, nil)))
	items := types.NewArray(observer.doc.Branch("items"))
	applyUntil(t, observer, items, 2)
	if items.Len() != 2 {
		t.Fatalf("observer sees %d items, want 2 (the valid edits)", items.Len())
	}
	if got := types.NewText(observer.doc.Branch("t")).String(); got != "" {
		t.Fatalf("observer text %q: the refused update reached the document", got)
	}
}

// readUntilItems applies the document updates c receives until its "items"
// array holds n elements, and fails if any of them does not decode
// strictly: the refused update must not reach c.
func readUntilItems(t *testing.T, name string, c *wsClient, n int) {
	t.Helper()
	arr := types.NewArray(c.doc.Branch("items"))
	deadline := time.Now().Add(3 * time.Second)
	for arr.Len() < uint64(n) && time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(c.ctx, 300*time.Millisecond)
		_, raw, err := c.conn.Read(ctx)
		cancel()
		if err != nil {
			continue
		}
		f, _, err := syncpkg.DecodeEnvelope(raw)
		if err != nil || f.Type != syncpkg.MessageSync || len(f.Payload) == 0 ||
			(f.SyncSub != syncpkg.SyncStep2 && f.SyncSub != syncpkg.SyncUpdate) {
			continue
		}
		if err := encoding.ApplyUpdate(c.doc, f.Payload); err != nil {
			t.Fatalf("%s received an update it cannot decode (the refused one?): %v", name, err)
		}
	}
	if arr.Len() != uint64(n) {
		t.Fatalf("%s has %d items, want %d", name, arr.Len(), n)
	}
}

// readUntilClosed reads frames until the server closes the connection and
// returns the close code.
func readUntilClosed(t *testing.T, c *wsClient) websocket.StatusCode {
	t.Helper()
	ctx, cancel := context.WithTimeout(c.ctx, 3*time.Second)
	defer cancel()
	for {
		if _, _, err := c.conn.Read(ctx); err != nil {
			if websocket.CloseStatus(err) == -1 {
				t.Logf("read ended without a close frame: %v", err)
			}
			return websocket.CloseStatus(err)
		}
	}
}

// An awareness message whose state holds invalid UTF-8 closes the sender's
// connection too: y-protocols reads the state with lib0's fatal decoder.
func TestServer_RefusesInvalidUTF8Awareness(t *testing.T) {
	wsURL, _ := startTestServer(t, server.Options{OriginPatterns: []string{"*"}})
	bad := dialClient(t, wsURL, "room", 1)
	defer bad.close()
	bad.read(t)
	// One client (1), clock 1, state "\xff" (a JSON string with one invalid byte).
	payload := []byte{0x01, 0x01, 0x01, 0x03, '"', 0xff, '"'}
	bad.write(t, syncpkg.EncodeAwareness(payload))
	if code := readUntilClosed(t, bad); code != websocket.StatusInternalError {
		t.Fatalf("closed with %v, want %v", code, websocket.StatusInternalError)
	}
}
