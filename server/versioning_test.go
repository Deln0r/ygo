package server

import (
	"context"
	"testing"
	"time"

	"github.com/Deln0r/ygo/internal/doc"
	"github.com/Deln0r/ygo/internal/encoding"
	"github.com/Deln0r/ygo/internal/memstore"
	"github.com/Deln0r/ygo/internal/types"
	"github.com/Deln0r/ygo/persist"
)

func mapSetUpdate(t *testing.T, clientID uint64, key, value string) []byte {
	t.Helper()
	d := doc.NewDocWithOptions(doc.Options{ClientID: clientID})
	m := types.NewMap(d.Branch("m"))
	txn := d.WriteTxn()
	m.Set(txn, key, value)
	txn.Commit()
	return encoding.EncodeStateAsUpdate(d)
}

// TestSweepVersions_DirtyOnly confirms a sweep versions exactly the
// dirty documents, clears the dirty set, and prunes to KeepVersions.
func TestSweepVersions_DirtyOnly(t *testing.T) {
	store, err := memstore.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	for _, name := range []string{"dirty-doc", "idle-doc"} {
		if err := store.StoreUpdate(ctx, name, mapSetUpdate(t, 300, "k", "v")); err != nil {
			t.Fatal(err)
		}
	}

	s := New(Options{Store: store, VersionInterval: time.Hour, KeepVersions: 2})
	defer s.Close(ctx)

	s.markVersionDirty("dirty-doc")
	s.sweepVersions(ctx, store)

	if vs, _ := store.ListVersions(ctx, "dirty-doc"); len(vs) != 1 {
		t.Errorf("dirty-doc versions = %d, want 1", len(vs))
	}
	if vs, _ := store.ListVersions(ctx, "idle-doc"); len(vs) != 0 {
		t.Errorf("idle-doc versions = %d, want 0 (was never dirty)", len(vs))
	}

	// A second sweep with nothing dirty captures nothing new.
	s.sweepVersions(ctx, store)
	if vs, _ := store.ListVersions(ctx, "dirty-doc"); len(vs) != 1 {
		t.Errorf("after idle sweep versions = %d, want still 1", len(vs))
	}

	// Three more dirty sweeps: prune holds the count at KeepVersions.
	for i := 0; i < 3; i++ {
		s.markVersionDirty("dirty-doc")
		s.sweepVersions(ctx, store)
	}
	if vs, _ := store.ListVersions(ctx, "dirty-doc"); len(vs) != 2 {
		t.Errorf("after prune versions = %d, want KeepVersions=2", len(vs))
	}
}

// TestVersioningLoop_StartStop confirms the ticker goroutine runs
// sweeps and shuts down cleanly via Close.
func TestVersioningLoop_StartStop(t *testing.T) {
	store, err := memstore.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	if err := store.StoreUpdate(ctx, "doc", mapSetUpdate(t, 301, "k", "v")); err != nil {
		t.Fatal(err)
	}

	s := New(Options{Store: store, VersionInterval: 10 * time.Millisecond, KeepVersions: 5})
	s.markVersionDirty("doc")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if vs, _ := store.ListVersions(ctx, "doc"); len(vs) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if vs, _ := store.ListVersions(ctx, "doc"); len(vs) == 0 {
		t.Error("ticker sweep never captured a version")
	}

	if err := s.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Second Close must not panic on the already-stopped loop.
	if err := s.Close(ctx); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestVersioning_DisabledWithoutInterval confirms the zero-value path
// stays inert: no goroutine, dirty marks are no-ops.
func TestVersioning_DisabledWithoutInterval(t *testing.T) {
	store, err := memstore.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	s := New(Options{Store: store})
	s.markVersionDirty("doc") // must not allocate or panic
	if s.versionDirty != nil {
		t.Error("dirty set allocated with versioning disabled")
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// Interface satisfaction: the test store must be a persist.VersionedStore
// or auto-versioning silently degrades (persist/sqlite checks its own).
var _ persist.VersionedStore = (*memstore.Store)(nil)
