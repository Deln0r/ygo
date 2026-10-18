package server_test

import (
	"encoding/hex"
	"testing"

	"github.com/Deln0r/ygo/internal/encoding"
	syncpkg "github.com/Deln0r/ygo/internal/sync"
	"github.com/Deln0r/ygo/internal/types"
	"github.com/Deln0r/ygo/server"
)

// An OnLoadDocument seed is the application's stored bytes: one that holds a
// string with invalid UTF-8 (text "\xff" in root "t") loads as it did before
// v1.26, instead of aborting the connection, and the room serves it.
func TestServer_OnLoadDocument_StoredInvalidUTF8Seed(t *testing.T) {
	seed, err := hex.DecodeString("010101000401017401ff00")
	if err != nil {
		t.Fatal(err)
	}
	wsURL, _ := startTestServer(t, server.Options{
		OriginPatterns: []string{"*"},
		OnLoadDocument: func(string) ([]byte, error) { return seed, nil },
	})
	c := dialClient(t, wsURL, "room", 1)
	defer c.close()
	c.read(t)
	c.write(t, syncpkg.EncodeSyncStep1(encoding.EncodeStateVector(map[uint64]uint64{}, nil)))
	f := c.readUntil(t, func(f *syncpkg.Frame) bool {
		return f.Type == syncpkg.MessageSync && f.SyncSub == syncpkg.SyncStep2
	})
	if err := encoding.ApplyStoredUpdate(c.doc, f.Payload); err != nil {
		t.Fatal(err)
	}
	if got := types.NewText(c.doc.Branch("t")).String(); got != "\xff" {
		t.Fatalf("served text %q, want the seed's byte", got)
	}
}
