package sqlite_test

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Deln0r/ygo/persist/sqlite"
)

// TestStore_SpecialCharPath_NoMisdirect guards against the file:-URI
// footgun: a path containing '#' (also '?' or '%') must be treated as a
// literal filename, not a URI, so writes are not silently misdirected to
// a phantom file. The load-bearing check is that the intended file
// exists — a misdirected store would round-trip fine through the wrong
// file and pass a naive read-back assertion.
func TestStore_SpecialCharPath_NoMisdirect(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "weird#name.db")

	s, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	if err := s.StoreUpdate(ctx, "d", []byte{1, 2}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetUpdates(ctx, "d")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("GetUpdates = %d, want 1", len(got))
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("intended db file %q missing (write misdirected by URI parsing?): %v", path, err)
	}
}

// TestStore_ConcurrentWrites_NoBusyErrors hammers a single on-disk
// database with concurrent writers. Without a busy timeout the file lock
// makes contending writes fail with SQLITE_BUSY ("database is locked"),
// silently losing updates; with it they queue and all land.
func TestStore_ConcurrentWrites_NoBusyErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "concurrent.db")
	s, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	const writers = 8
	const perWriter = 40
	ctx := context.Background()

	var wg sync.WaitGroup
	errs := make(chan error, writers*perWriter)
	for g := 0; g < writers; g++ {
		g := g
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				blob := []byte{byte(g), byte(i), 0x00, 0x00}
				if err := s.StoreUpdate(ctx, "doc", blob); err != nil {
					errs <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("StoreUpdate under concurrency: %v", err)
	}

	got, err := s.GetUpdates(ctx, "doc")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != writers*perWriter {
		t.Errorf("GetUpdates returned %d updates, want %d (writes lost)", len(got), writers*perWriter)
	}
}
