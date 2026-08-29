package persist_test

import (
	"context"
	"encoding/hex"
	"errors"
	"testing"

	ygo "github.com/Deln0r/ygo"
	"github.com/Deln0r/ygo/persist"
	"github.com/Deln0r/ygo/persist/sqlite"
)

// Two consecutive transaction updates captured from real yjs@13.6.32
// (doc.on("update") on a Y.Doc with clientID=42: push "A", then push "B").
// uB carries ONLY B's block, whose origin references A's - the exact shape a
// y-websocket client broadcasts. A log holding uB without uA is causally
// incomplete: reachable in the server because an update is applied and
// broadcast BEFORE it is persisted, so a fast peer's dependent edit can
// reach the Store first.
const (
	danglingUA = "01012a000801056974656d730177014100"
	danglingUB = "01012a01882a000177014200"
)

func mustHex(t *testing.T, h string) []byte {
	t.Helper()
	b, err := hex.DecodeString(h)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func itemsLen(t *testing.T, blobs ...[]byte) int {
	t.Helper()
	d := ygo.NewDoc()
	for _, b := range blobs {
		if len(b) == 0 {
			continue
		}
		if err := ygo.ApplyUpdate(d, b); err != nil {
			t.Fatalf("apply: %v", err)
		}
	}
	arr := ygo.NewArray(d, "items")
	rt := d.ReadTxn()
	defer rt.Close()
	return int(arr.Len())
}

// TestMergeUpdates_RefusesIncompleteLog is the data-loss regression: before
// the fix, MergeUpdates of a log holding only the dependent update returned
// an EMPTY snapshot (the pending block was silently dropped), and the
// destructive replace in Flush then erased the original bytes forever. Now
// it refuses with ErrIncompleteLog, and the complete log still compacts.
func TestMergeUpdates_RefusesIncompleteLog(t *testing.T) {
	uA, uB := mustHex(t, danglingUA), mustHex(t, danglingUB)
	if got := itemsLen(t, uB, uA); got != 2 {
		t.Fatalf("sanity: out-of-order replay len=%d, want 2", got)
	}

	if _, err := persist.MergeUpdates([][]byte{uB}); !errors.Is(err, persist.ErrIncompleteLog) {
		t.Fatalf("MergeUpdates(dangling) err=%v, want ErrIncompleteLog", err)
	}

	snap, err := persist.MergeUpdates([][]byte{uB, uA})
	if err != nil {
		t.Fatalf("MergeUpdates(complete) err=%v", err)
	}
	if got := itemsLen(t, snap); got != 2 {
		t.Fatalf("compacted complete log replays len=%d, want 2", got)
	}
}

// TestSQLiteFlush_IncompleteLogSurvives drives the same shape through the
// real store: a Flush over the causally incomplete log must refuse, leave
// the original bytes in place, and succeed once the ancestor lands.
func TestSQLiteFlush_IncompleteLogSurvives(t *testing.T) {
	uA, uB := mustHex(t, danglingUA), mustHex(t, danglingUB)
	s, err := sqlite.Open(t.TempDir() + "/x.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	// An unrelated, self-contained update from another client: the log needs
	// two entries because Flush short-circuits a single-update log without
	// merging (already optimal - and, usefully, incapable of losing anything).
	other := ygo.NewDocWithOptions(ygo.Options{ClientID: 7})
	om := ygo.NewMap(other, "other")
	otxn := other.WriteTxn()
	om.Set(otxn, "k", "v")
	otxn.Commit()
	uC := ygo.EncodeStateAsUpdate(other)

	if err := s.StoreUpdate(ctx, "d", uB); err != nil { // dependent, ancestor absent
		t.Fatal(err)
	}
	if err := s.StoreUpdate(ctx, "d", uC); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(ctx, "d"); !errors.Is(err, persist.ErrIncompleteLog) {
		t.Fatalf("Flush(incomplete) err=%v, want ErrIncompleteLog", err)
	}
	got, err := s.GetUpdates(ctx, "d")
	if err != nil || len(got) != 2 || itemsLen(t, got[0], got[1], uA) != 2 {
		t.Fatalf("refused flush must leave the log intact: n=%d err=%v", len(got), err)
	}

	if err := s.StoreUpdate(ctx, "d", uA); err != nil { // ancestor lands
		t.Fatal(err)
	}
	if err := s.Flush(ctx, "d"); err != nil {
		t.Fatalf("Flush(complete) err=%v", err)
	}
	got, err = s.GetUpdates(ctx, "d")
	if err != nil {
		t.Fatal(err)
	}
	if got2 := itemsLen(t, got...); got2 != 2 {
		t.Fatalf("post-flush replay len=%d, want 2 (compaction lost a dependent update)", got2)
	}
	if len(got) != 1 {
		t.Fatalf("complete log compacted into %d blobs, want 1", len(got))
	}
}
