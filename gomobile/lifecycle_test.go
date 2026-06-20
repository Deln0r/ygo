package gomobile_test

import (
	"sync"
	"testing"

	"github.com/Deln0r/ygo/gomobile"
)

// TestMobile_ConcurrentConnectClose fires Connect and Close concurrently
// on a fresh offline-store-backed mobile client — the fast screen-dismiss
// pattern (viewDidDisappear firing Close while a connect task is still
// scheduling). The teardown must be race-free under -race and must not
// leak the inner goroutines or run loadLocal against a store Close has
// already closed. Before the fix Connect called inner.Connect() (which
// opens and reads the sqlite store) outside c.mu, racing Close's
// store.Close().
func TestMobile_ConcurrentConnectClose(t *testing.T) {
	url := startServer(t)
	for i := 0; i < 30; i++ {
		d := gomobile.NewDocWithClientID(uint64(i + 1))
		c := gomobile.NewClient(url, "room", d)
		c.EnableOfflineStore(t.TempDir() + "/m.db")

		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _ = c.Connect() }()
		go func() { defer wg.Done(); _ = c.Close() }()
		wg.Wait()

		// Trailing Close is a no-op and joins anything Connect started.
		_ = c.Close()
	}
}
