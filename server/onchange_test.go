package server_test

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/Deln0r/ygo/internal/encoding"
	syncpkg "github.com/Deln0r/ygo/internal/sync"
	"github.com/Deln0r/ygo/server"
)

// failingStore is a persist.Store whose StoreUpdate always errors; the
// rest are inert. Used to prove OnChange fires even when persistence
// fails (the document still changed in memory and on peers).
type failingStore struct{}

func (failingStore) StoreUpdate(context.Context, string, []byte) error {
	return errors.New("failingStore: StoreUpdate always errors")
}
func (failingStore) GetUpdates(context.Context, string) ([][]byte, error) { return nil, nil }
func (failingStore) Flush(context.Context, string) error                  { return nil }
func (failingStore) DocumentExists(context.Context, string) (bool, error) { return false, nil }
func (failingStore) ListDocuments(context.Context) ([]string, error)      { return nil, nil }
func (failingStore) ClearDocument(context.Context, string) error          { return nil }
func (failingStore) Close() error                                         { return nil }

// TestServer_OnChange_FiresForAppliedUpdatesOnly checks the OnChange
// hook fires once per applied document update (with the docName and a
// non-empty update), independent of any Store, and does NOT fire for a
// read-only connection's dropped edit or for an awareness frame.
func TestServer_OnChange_FiresForAppliedUpdatesOnly(t *testing.T) {
	var mu sync.Mutex
	var changes []string
	var lastLen int

	// No Store configured: OnChange must still fire (it is independent of
	// persistence).
	wsURL, _ := startTestServer(t, server.Options{
		OriginPatterns: []string{"*"},
		ReadOnly: func(_ string, r *http.Request) bool {
			return r.URL.Query().Get("ro") == "1"
		},
		OnChange: func(docName string, update []byte) {
			mu.Lock()
			changes = append(changes, docName)
			lastLen = len(update)
			mu.Unlock()
		},
	})

	editor := dialClient(t, wsURL, "doc", 1)
	defer editor.close()
	editor.read(t)
	viewer := dialClient(t, wsURL, "doc?ro=1", 2)
	defer viewer.close()
	viewer.read(t)

	// An editor edit fires OnChange once, with the docName and a
	// non-empty update.
	pushItem(t, editor, "items", "x")
	waitFor(t, 3*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(changes) == 1
	})
	mu.Lock()
	if changes[0] != "doc" {
		t.Errorf("OnChange docName = %q, want %q", changes[0], "doc")
	}
	if lastLen == 0 {
		t.Error("OnChange update was empty")
	}
	mu.Unlock()

	// A read-only viewer's edit is dropped, so OnChange must not fire
	// again. Sending the viewer's own SyncStep1 after the edit forces the
	// server to process (and drop) the edit frame in order first.
	pushItem(t, viewer, "items", "y")
	viewer.write(t, syncpkg.EncodeSyncStep1(encoding.EncodeStateVector(map[uint64]uint64{}, nil)))
	viewer.readUntil(t, func(f *syncpkg.Frame) bool {
		return f.Type == syncpkg.MessageSync && f.SyncSub == syncpkg.SyncStep2
	})

	// An awareness frame must not fire OnChange either.
	editor.awareness.SetLocalState([]byte(`{"cursor":1}`))
	editor.write(t, syncpkg.EncodeAwareness(editor.awareness.Encode(nil)))
	editor.write(t, syncpkg.EncodeSyncStep1(encoding.EncodeStateVector(map[uint64]uint64{}, nil)))
	editor.readUntil(t, func(f *syncpkg.Frame) bool {
		return f.Type == syncpkg.MessageSync && f.SyncSub == syncpkg.SyncStep2
	})

	mu.Lock()
	defer mu.Unlock()
	if len(changes) != 1 {
		t.Errorf("OnChange fired %d times, want 1 (only the editor's applied edit)", len(changes))
	}
}

// TestServer_OnChange_FiresEvenWhenStoreUpdateFails proves the documented
// surprising case: OnChange fires after a failed StoreUpdate, because the
// document already changed in memory and on peers. It must not be
// suppressed by the persistence error.
func TestServer_OnChange_FiresEvenWhenStoreUpdateFails(t *testing.T) {
	var mu sync.Mutex
	var count int

	wsURL, _ := startTestServer(t, server.Options{
		OriginPatterns: []string{"*"},
		Store:          failingStore{},
		OnChange: func(_ string, _ []byte) {
			mu.Lock()
			count++
			mu.Unlock()
		},
	})

	editor := dialClient(t, wsURL, "doc", 1)
	defer editor.close()
	editor.read(t)

	pushItem(t, editor, "items", "x")
	waitFor(t, 3*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return count >= 1
	})
	mu.Lock()
	defer mu.Unlock()
	if count != 1 {
		t.Errorf("OnChange fired %d times, want 1 (must fire despite StoreUpdate error)", count)
	}
}
