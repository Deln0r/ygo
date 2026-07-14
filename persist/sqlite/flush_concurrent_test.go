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
