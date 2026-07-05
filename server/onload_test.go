package server_test

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Deln0r/ygo/internal/doc"
	"github.com/Deln0r/ygo/internal/encoding"
	syncpkg "github.com/Deln0r/ygo/internal/sync"
	"github.com/Deln0r/ygo/internal/types"
	"github.com/Deln0r/ygo/persist/sqlite"
	"github.com/Deln0r/ygo/server"
)

// TestServer_OnLoadDocument_SeedsFirstLoadOnly checks OnLoadDocument
// seeds a fresh document's content on first load and is not called again
// for a cache hit (a second connection to the still-resident room).
func TestServer_OnLoadDocument_SeedsFirstLoadOnly(t *testing.T) {
	// Pre-built seed update: one item in "items".
	src := doc.NewDocWithOptions(doc.Options{ClientID: 999})
	sa := types.NewArray(src.Branch("items"))
	stx := src.WriteTxn()
	sa.Push(stx, "seed")
	stx.Commit()
	seed := encoding.EncodeStateAsUpdate(src)

	var mu sync.Mutex
	var loads []string
	wsURL, _ := startTestServer(t, server.Options{
		OriginPatterns: []string{"*"},
		OnLoadDocument: func(docName string) ([]byte, error) {
			mu.Lock()
			loads = append(loads, docName)
			mu.Unlock()
			return seed, nil
		},
	})

	// First client to "room" triggers the load and pulls the seed.
	c := dialClient(t, wsURL, "room", 1)
	defer c.close()
	c.read(t)
	c.write(t, syncpkg.EncodeSyncStep1(encoding.EncodeStateVector(map[uint64]uint64{}, nil)))
	arr := types.NewArray(c.doc.Branch("items"))
	applyUntil(t, c, arr, 1)
	if arr.Len() != 1 {
		t.Fatalf("seeded doc len = %d, want 1", arr.Len())
	}

	// A second client to the SAME resident room is a cache hit: the
	// document is not reloaded, so OnLoadDocument must not fire again.
	c2 := dialClient(t, wsURL, "room", 2)
	defer c2.close()
	c2.read(t)

	mu.Lock()
	n := len(loads)
	mu.Unlock()
	if n != 1 {
		t.Errorf("OnLoadDocument called %d times, want 1 (only the first load)", n)
	}
}

// TestServer_OnLoadDocument_ErrorAbortsConnection checks a load error
// aborts the connection with StatusInternalError and leaves no document
// registered (the connection simply fails to open).
func TestServer_OnLoadDocument_ErrorAbortsConnection(t *testing.T) {
	wsURL, _ := startTestServer(t, server.Options{
		OriginPatterns: []string{"*"},
		OnLoadDocument: func(string) ([]byte, error) {
			return nil, errors.New("load denied")
		},
	})
	if got := dialExpectClose(t, wsURL, "any"); got != websocket.StatusInternalError {
		t.Fatalf("close status = %d, want %d (InternalError)", got, websocket.StatusInternalError)
	}
}

// TestServer_OnLoadDocument_StableSeed_SurvivesEvictionWithStore combines
// OnLoadDocument with a Store across an eviction and reload, using a
// stable-ID seed (built once, reused): the seed re-applies idempotently
// so the reloaded document holds exactly the seed plus the persisted
// client edit, with no duplication and no loss.
func TestServer_OnLoadDocument_StableSeed_SurvivesEvictionWithStore(t *testing.T) {
	// Fixed seed: built once with a reserved ClientID, returned verbatim
	// each call, so its item IDs are stable across reloads.
	src := doc.NewDocWithOptions(doc.Options{ClientID: 999})
	sa := types.NewArray(src.Branch("items"))
	stx := src.WriteTxn()
	sa.Push(stx, "seed")
	stx.Commit()
	seed := encoding.EncodeStateAsUpdate(src)

	dbPath := filepath.Join(t.TempDir(), "onload.db")
	store, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	wsURL, _ := startTestServer(t, server.Options{
		OriginPatterns: []string{"*"},
		Store:          store,
		OnLoadDocument: func(string) ([]byte, error) { return seed, nil },
	})

	// Round 1: first load seeds the doc; a client adds an edit that is
	// persisted. Pull round-trip forces the server to apply+persist the
	// edit in order before we close.
	c1 := dialClient(t, wsURL, "room", 1)
	c1.read(t)
	pushItem(t, c1, "items", "edit")
	c1.write(t, syncpkg.EncodeSyncStep1(encoding.EncodeStateVector(map[uint64]uint64{}, nil)))
	c1.readUntil(t, func(f *syncpkg.Frame) bool {
		return f.Type == syncpkg.MessageSync && f.SyncSub == syncpkg.SyncStep2
	})
	c1.close()
	time.Sleep(150 * time.Millisecond) // let the eviction flush land

	// Round 2: reconnect forces a fresh first-load (Store history +
	// OnLoadDocument re-seed). The seed must not duplicate.
	c2 := dialClient(t, wsURL, "room", 2)
	defer c2.close()
	c2.read(t)
	c2.write(t, syncpkg.EncodeSyncStep1(encoding.EncodeStateVector(map[uint64]uint64{}, nil)))
	arr := types.NewArray(c2.doc.Branch("items"))
	applyUntil(t, c2, arr, 2)
	if arr.Len() != 2 {
		t.Fatalf("after eviction+reload: items = %d, want 2 (seed + edit, no duplication or loss)", arr.Len())
	}
}
