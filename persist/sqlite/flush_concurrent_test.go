package sqlite_test

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Deln0r/ygo/internal/doc"
	"github.com/Deln0r/ygo/internal/encoding"
	"github.com/Deln0r/ygo/internal/types"
	"github.com/Deln0r/ygo/persist"
	"github.com/Deln0r/ygo/persist/sqlite"
)

// mapUpdate builds a valid single-key V1 update from an independent doc
// with a unique client, so a set of them merges into the union of their
// keys (distinct clients + keys never conflict).
func mapUpdate(t *testing.T, client uint64, key, val string) []byte {
	t.Helper()
	d := doc.NewDocWithOptions(doc.Options{ClientID: client})
	m := types.NewMap(d.Branch("settings"))
	txn := d.WriteTxn()
	m.Set(txn, key, val)
	txn.Commit()
	return encoding.EncodeStateAsUpdate(d)
}

// TestStore_RestoreVersionConcurrentWithWrites_NoBusy exercises
// RestoreVersion racing StoreUpdate on an on-disk (WAL) store: the
// BEGIN IMMEDIATE transaction takes the write lock up front, so a
// concurrent writer blocks (bounded by busy_timeout) instead of
// invalidating the restore's read snapshot and failing it with
// SQLITE_BUSY_SNAPSHOT — the same defect class fixed in Flush. Asserts
// no transient busy failures on either side and that the restored base
// state survives every interleaving.
func TestStore_RestoreVersionConcurrentWithWrites_NoBusy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "restorerace.db")
	s, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	// Seed the doc and capture its state as the version to restore.
	base := mapUpdate(t, 1, "base", "v")
	if err := s.StoreUpdate(ctx, "doc", base); err != nil {
		t.Fatal(err)
	}
	verID, err := s.SaveVersion(ctx, "doc", "checkpoint", base)
	if err != nil {
		t.Fatal(err)
	}

	const writers = 4
	const perWriter = 20
	const restorers = 2
	const perRestorer = 30

	var wg sync.WaitGroup
	writeErrs := make(chan error, writers*perWriter)
	restoreErrs := make(chan error, restorers*perRestorer)
	for g := 0; g < writers; g++ {
		g := g
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				u := mapUpdate(t, uint64(g*1000+i+2), fmt.Sprintf("w_%d_%d", g, i), "v")
				if err := s.StoreUpdate(ctx, "doc", u); err != nil {
					writeErrs <- err
				}
			}
		}()
	}
	for r := 0; r < restorers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perRestorer; i++ {
				if err := s.RestoreVersion(ctx, "doc", verID); err != nil {
					restoreErrs <- err
				}
			}
		}()
	}
	wg.Wait()
	close(writeErrs)
	close(restoreErrs)
	for err := range writeErrs {
		t.Errorf("StoreUpdate under concurrent restore: %v", err)
	}
	for err := range restoreErrs {
		t.Errorf("RestoreVersion under concurrent writes (BEGIN IMMEDIATE should let busy_timeout absorb this): %v", err)
	}

	// The restored base state must be present regardless of interleaving
	// (each restore rewrites the log to start from it; later appends stack
	// on top). The log must always reconstruct.
	loaded, err := persist.LoadDoc(ctx, s, "doc", doc.Options{})
	if err != nil {
		t.Fatal(err)
	}
	lm := types.NewMap(loaded.Branch("settings"))
	rtxn := loaded.ReadTxn()
	defer rtxn.Close()
	if got := lm.Get("base"); got != "v" {
		t.Errorf("post-run Get(base) = %v, want v (restored state lost)", got)
	}
}

// TestStore_FlushConcurrentWithWrites_NoLossNoBusy exercises the exact
// path Server.Flush relies on: Flush called repeatedly on a live, on-disk
// (WAL) document while StoreUpdate writers append concurrently. It asserts
// the two properties the public API promises:
//
//   - no data loss: every written key survives the interleaved
//     compactions (Flush deletes only what it folded into the snapshot);
//   - no transient busy failure: Flush's BEGIN IMMEDIATE routes contention
//     through busy_timeout, so it does not fail with SQLITE_BUSY_SNAPSHOT.
//
// A deferred-transaction Flush (the pre-fix code) fails a large fraction of
// these flushes; this test is the regression guard for that fix. Run under
// -race -count to shuffle the interleaving.
func TestStore_FlushConcurrentWithWrites_NoLossNoBusy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "flushrace.db")
	s, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	const writers = 6
	const perWriter = 20
	const flushers = 3
	const perFlusher = 40

	// Seed so the log starts with more than one update (Flush is a no-op
	// below two, and we want it doing real read-delete-insert work).
	for i := 0; i < 2; i++ {
		if err := s.StoreUpdate(ctx, "doc", mapUpdate(t, uint64(900+i), fmt.Sprintf("seed%d", i), "v")); err != nil {
			t.Fatal(err)
		}
	}

	var wg sync.WaitGroup
	writeErrs := make(chan error, writers*perWriter)
	flushErrs := make(chan error, flushers*perFlusher)

	for g := 0; g < writers; g++ {
		g := g
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				client := uint64(g*1000 + i + 1)
				u := mapUpdate(t, client, fmt.Sprintf("k_%d_%d", g, i), "v")
				if err := s.StoreUpdate(ctx, "doc", u); err != nil {
					writeErrs <- err
				}
			}
		}()
	}
	for f := 0; f < flushers; f++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perFlusher; i++ {
				if err := s.Flush(ctx, "doc"); err != nil {
					flushErrs <- err
				}
			}
		}()
	}
	wg.Wait()
	close(writeErrs)
	close(flushErrs)

	for err := range writeErrs {
		t.Errorf("StoreUpdate under concurrent Flush: %v", err)
	}
	for err := range flushErrs {
		t.Errorf("Flush under concurrent writes (BEGIN IMMEDIATE should let busy_timeout absorb this): %v", err)
	}

	// No data loss: every written key survives every interleaved compaction.
	loaded, err := persist.LoadDoc(ctx, s, "doc", doc.Options{})
	if err != nil {
		t.Fatal(err)
	}
	lm := types.NewMap(loaded.Branch("settings"))
	rtxn := loaded.ReadTxn()
	defer rtxn.Close()
	for g := 0; g < writers; g++ {
		for i := 0; i < perWriter; i++ {
			key := fmt.Sprintf("k_%d_%d", g, i)
			if got := lm.Get(key); got != "v" {
				t.Errorf("post-run Get(%q) = %v, want v (update lost across Flush)", key, got)
			}
		}
	}
}
