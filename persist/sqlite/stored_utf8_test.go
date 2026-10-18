package sqlite_test

import (
	"context"
	"encoding/hex"
	"errors"
	"path/filepath"
	"testing"

	"github.com/Deln0r/ygo"
	"github.com/Deln0r/ygo/internal/doc"
	"github.com/Deln0r/ygo/internal/encoding"
	"github.com/Deln0r/ygo/internal/types"
	"github.com/Deln0r/ygo/persist"
	"github.com/Deln0r/ygo/persist/sqlite"
)

// storedInvalid is a V1 update that a release before v1.26 accepted and
// stored: one string item "\xff" in root text "t", client 1. lib0 rejects it
// (a fatal UTF-8 decoder), and so does ApplyUpdate now; a store holding it
// must still load it, with the byte unchanged.
const storedInvalid = "010101000401017401ff00"

// storedAppend is a second update from the same client, a valid "a" after
// the first item, so a flush has two updates to merge.
func storedAppend(t *testing.T) []byte {
	t.Helper()
	d := doc.NewDocWithOptions(doc.Options{ClientID: 1})
	txt := types.NewText(d.Branch("t"))
	if err := encoding.ApplyStoredUpdate(d, mustHex(t, storedInvalid)); err != nil {
		t.Fatal(err)
	}
	var out []byte
	d.OnUpdate(func(u []byte, _ any) { out = append([]byte(nil), u...) })
	w := d.WriteTxn()
	if err := txt.Insert(w, 1, "a"); err != nil {
		t.Fatal(err)
	}
	w.Commit()
	return out
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func textOf(t *testing.T, d *doc.Doc) string {
	t.Helper()
	return types.NewText(d.Branch("t")).String()
}

func TestStoredInvalidUTF8_LoadsAsBefore(t *testing.T) {
	raw := mustHex(t, storedInvalid)
	if err := ygo.ApplyUpdate(ygo.NewDoc(), raw); !errors.Is(err, ygo.ErrInvalidUTF8) {
		t.Fatalf("ApplyUpdate = %v, want ErrInvalidUTF8 (positive control: the bytes are rejected at ingress)", err)
	}

	ctx := context.Background()
	s, err := sqlite.Open(filepath.Join(t.TempDir(), "stored.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.StoreUpdate(ctx, "d", raw); err != nil {
		t.Fatal(err)
	}

	d, err := persist.LoadDoc(ctx, s, "d", doc.Options{})
	if err != nil {
		t.Fatalf("LoadDoc: %v", err)
	}
	if got := textOf(t, d); got != "\xff" {
		t.Fatalf("loaded text %q, want the stored byte unchanged", got)
	}

	// A flush merges the log; the merged update still holds the byte.
	if err := s.StoreUpdate(ctx, "d", storedAppend(t)); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(ctx, "d"); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	d, err = persist.LoadDoc(ctx, s, "d", doc.Options{})
	if err != nil {
		t.Fatalf("LoadDoc after flush: %v", err)
	}
	if got := textOf(t, d); got != "\xffa" {
		t.Fatalf("text after flush %q, want %q", got, "\xffa")
	}

	// A version of it saves and loads.
	id, err := persist.SaveVersion(ctx, s, "d", "v")
	if err != nil {
		t.Fatalf("SaveVersion: %v", err)
	}
	v, err := persist.LoadVersion(ctx, s, "d", id, doc.Options{})
	if err != nil {
		t.Fatalf("LoadVersion: %v", err)
	}
	if got := textOf(t, v); got != "\xffa" {
		t.Fatalf("version text %q, want %q", got, "\xffa")
	}

	// A snapshot of the loaded document restores.
	g, err := persist.LoadDoc(ctx, s, "d", doc.Options{DisableGC: true})
	if err != nil {
		t.Fatal(err)
	}
	restored, err := ygo.RestoreSnapshot(g, ygo.CreateSnapshot(g))
	if err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}
	if got := textOf(t, restored); got != "\xffa" {
		t.Fatalf("restored text %q, want %q", got, "\xffa")
	}
}
