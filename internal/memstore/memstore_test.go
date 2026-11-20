package memstore_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Deln0r/ygo/internal/memstore"
)

// TestOpen_PathSharesState: a path reopened after Close sees what was
// stored through it; ":memory:" and "" are private each time; a closed
// handle refuses work.
func TestOpen_PathSharesState(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/doc.db"
	a, _ := memstore.Open(path)
	if err := a.StoreUpdate(ctx, "d", []byte{1}); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := a.GetUpdates(ctx, "d"); !errors.Is(err, memstore.ErrClosed) {
		t.Fatalf("a closed store: %v", err)
	}
	b, _ := memstore.Open(path)
	if got, _ := b.GetUpdates(ctx, "d"); len(got) != 1 {
		t.Fatalf("the reopened path holds %d updates, want 1", len(got))
	}
	for _, p := range []string{":memory:", ""} {
		m1, _ := memstore.Open(p)
		if err := m1.StoreUpdate(ctx, "d", []byte{1}); err != nil {
			t.Fatal(err)
		}
		m2, _ := memstore.Open(p)
		if got, _ := m2.GetUpdates(ctx, "d"); len(got) != 0 {
			t.Fatalf("%q is shared between opens", p)
		}
	}
}
