package persist_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Deln0r/ygo/internal/doc"
	"github.com/Deln0r/ygo/internal/types"
	"github.com/Deln0r/ygo/persist"
)

// readKey loads the "settings" root map value for key from d.
func readKey(d *doc.Doc, key string) any {
	return types.NewMap(d.Branch("settings")).Get(key)
}

func TestSaveVersion_RoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.StoreUpdate(ctx, "doc", applyMapSet(t, 100, "a", "one")); err != nil {
		t.Fatal(err)
	}
	if err := s.StoreUpdate(ctx, "doc", applyMapSet(t, 101, "b", "two")); err != nil {
		t.Fatal(err)
	}

	id, err := persist.SaveVersion(ctx, s, "doc", "checkpoint")
	if err != nil {
		t.Fatalf("SaveVersion: %v", err)
	}

	infos, err := s.ListVersions(ctx, "doc")
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("got %d versions, want 1", len(infos))
	}
	if infos[0].ID != id || infos[0].Label != "checkpoint" || infos[0].Size == 0 {
		t.Errorf("info = %+v, want ID %d label %q non-zero size", infos[0], id, "checkpoint")
	}
	if infos[0].CreatedAt.IsZero() {
		t.Error("CreatedAt is zero")
	}

	v, err := persist.LoadVersion(ctx, s, "doc", id, doc.Options{})
	if err != nil {
		t.Fatalf("LoadVersion: %v", err)
	}
	if got := readKey(v, "a"); got != "one" {
		t.Errorf("a = %v, want one", got)
	}
	if got := readKey(v, "b"); got != "two" {
		t.Errorf("b = %v, want two", got)
	}
}

func TestSaveVersion_UnknownDocument(t *testing.T) {
	s := newTestStore(t)
	_, err := persist.SaveVersion(context.Background(), s, "ghost", "")
	if !errors.Is(err, persist.ErrUnknownDocument) {
		t.Errorf("err = %v, want ErrUnknownDocument", err)
	}
}

// TestVersions_SurviveFlushAndClear locks the independence contract:
// the live log can be compacted or cleared without touching history.
func TestVersions_SurviveFlushAndClear(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.StoreUpdate(ctx, "doc", applyMapSet(t, 110, "k", "v")); err != nil {
		t.Fatal(err)
	}
	id, err := persist.SaveVersion(ctx, s, "doc", "kept")
	if err != nil {
		t.Fatal(err)
	}

	if err := s.Flush(ctx, "doc"); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if err := s.ClearDocument(ctx, "doc"); err != nil {
		t.Fatalf("ClearDocument: %v", err)
	}

	v, err := persist.LoadVersion(ctx, s, "doc", id, doc.Options{})
	if err != nil {
		t.Fatalf("LoadVersion after clear: %v", err)
	}
	if got := readKey(v, "k"); got != "v" {
		t.Errorf("k = %v, want v", got)
	}
}

func TestRestoreVersion_RewindsLiveLog(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.StoreUpdate(ctx, "doc", applyMapSet(t, 120, "k", "old")); err != nil {
		t.Fatal(err)
	}
	id, err := persist.SaveVersion(ctx, s, "doc", "before")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.StoreUpdate(ctx, "doc", applyMapSet(t, 121, "k", "new")); err != nil {
		t.Fatal(err)
	}

	if err := s.RestoreVersion(ctx, "doc", id); err != nil {
		t.Fatalf("RestoreVersion: %v", err)
	}

	d, err := persist.LoadDoc(ctx, s, "doc", doc.Options{})
	if err != nil {
		t.Fatalf("LoadDoc: %v", err)
	}
	if got := readKey(d, "k"); got != "old" {
		t.Errorf("k = %v, want old (post-version update rewound)", got)
	}
	updates, err := s.GetUpdates(ctx, "doc")
	if err != nil {
		t.Fatal(err)
	}
	if len(updates) != 1 {
		t.Errorf("log has %d blobs after restore, want 1", len(updates))
	}
}

func TestRestoreVersion_NotFound(t *testing.T) {
	s := newTestStore(t)
	err := s.RestoreVersion(context.Background(), "doc", 999)
	if !errors.Is(err, persist.ErrVersionNotFound) {
		t.Errorf("err = %v, want ErrVersionNotFound", err)
	}
}

func TestGetVersionState_NotFound(t *testing.T) {
	s := newTestStore(t)
	_, err := s.GetVersionState(context.Background(), "doc", 999)
	if !errors.Is(err, persist.ErrVersionNotFound) {
		t.Errorf("err = %v, want ErrVersionNotFound", err)
	}
}

func TestDeleteVersion_Idempotent(t *testing.T) {
	s := newTestStore(t)
	if err := s.DeleteVersion(context.Background(), "doc", 999); err != nil {
		t.Errorf("deleting unknown version: %v", err)
	}
}

func TestPruneVersions_KeepsNewest(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.StoreUpdate(ctx, "doc", applyMapSet(t, 130, "k", "v")); err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for i := 0; i < 5; i++ {
		id, err := persist.SaveVersion(ctx, s, "doc", "")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}

	deleted, err := persist.PruneVersions(ctx, s, "doc", 2)
	if err != nil {
		t.Fatalf("PruneVersions: %v", err)
	}
	if deleted != 3 {
		t.Errorf("deleted = %d, want 3", deleted)
	}
	infos, err := s.ListVersions(ctx, "doc")
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 2 || infos[0].ID != ids[3] || infos[1].ID != ids[4] {
		t.Errorf("survivors = %+v, want IDs %d, %d", infos, ids[3], ids[4])
	}

	// Idempotent: nothing more to prune.
	deleted, err = persist.PruneVersions(ctx, s, "doc", 2)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 0 {
		t.Errorf("second prune deleted %d, want 0", deleted)
	}
}

func TestSaveVersion_EmptyStateRejected(t *testing.T) {
	s := newTestStore(t)
	_, err := s.SaveVersion(context.Background(), "doc", "x", nil)
	if !errors.Is(err, persist.ErrEmptyUpdate) {
		t.Errorf("err = %v, want ErrEmptyUpdate", err)
	}
}

// LoadVersion with a fixed ClientID that the version's state wrote under
// gives the loaded doc a new random ID, as LoadDoc does.
func TestLoadVersion_FixedClientIDInTheStateIsReplaced(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.StoreUpdate(ctx, "doc", applyMapSet(t, 7, "k", "v")); err != nil {
		t.Fatal(err)
	}
	id, err := persist.SaveVersion(ctx, s, "doc", "")
	if err != nil {
		t.Fatal(err)
	}
	d, err := persist.LoadVersion(ctx, s, "doc", id, doc.Options{ClientID: 7})
	if err != nil {
		t.Fatal(err)
	}
	if d.ClientID() == 7 {
		t.Fatal("the loaded version kept client ID 7, which its state wrote under")
	}
}
