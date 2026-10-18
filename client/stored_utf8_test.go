package client_test

import (
	"context"
	"encoding/hex"
	"testing"

	"github.com/Deln0r/ygo/internal/types"
)

// An update a release before v1.26 stored in the client's LocalStore with a
// string of invalid UTF-8 (text "\xff" in root "t") still loads into the
// document on Connect, byte for byte, although ApplyUpdate now refuses such
// bytes from the network.
func TestClient_LocalStoreKeepsStoredInvalidUTF8(t *testing.T) {
	path := t.TempDir() + "/local.db"
	st := openStore(t, path)
	t.Cleanup(func() { _ = st.Close() })
	raw, err := hex.DecodeString("010101000401017401ff00")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.StoreUpdate(context.Background(), "room", raw); err != nil {
		t.Fatal(err)
	}
	c := connectClient(t, deadURL(t), path, 2, st)
	t.Cleanup(func() { _ = c.Close() })
	if got := types.NewText(c.Doc().Branch("t")).String(); got != "\xff" {
		t.Fatalf("loaded text %q, want the stored byte unchanged", got)
	}
}
