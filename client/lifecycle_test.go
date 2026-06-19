package client_test

import (
	"context"
	"sync"
	"testing"

	"github.com/Deln0r/ygo/client"
	"github.com/Deln0r/ygo/internal/doc"
)

// TestClient_ConcurrentConnectClose fires Connect and Close concurrently
// on a fresh, offline-store-backed client. The teardown must be race-free
// (run under -race) and must never hang: whichever wins, the loops are
// either fully started-then-stopped or never started, with the observer
// always removed. Before the fix Connect published c.unsubscribe /
// c.persistDone outside c.mu, racing Close's reads and leaking the
// observer + goroutines on a Close-during-setup interleave.
func TestClient_ConcurrentConnectClose(t *testing.T) {
	for i := 0; i < 30; i++ {
		path := t.TempDir() + "/local.db"
		st := openStore(t, path)

		c, err := client.New(client.Options{
			URL:        deadURL(t), // never connects; exercises the offline setup path
			DocName:    "room",
			Doc:        doc.NewDocWithOptions(doc.Options{ClientID: uint64(i + 1)}),
			LocalStore: st,
		})
		if err != nil {
			t.Fatal(err)
		}

		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _ = c.Connect(context.Background()) }()
		go func() { defer wg.Done(); _ = c.Close() }()
		wg.Wait()

		// A trailing Close is a no-op and joins any loops Connect started.
		if err := c.Close(); err != nil {
			t.Fatalf("trailing Close: %v", err)
		}
		_ = st.Close()
	}
}
