package sqlite

import (
	"context"
	"path/filepath"
	"testing"
)

// TestOpen_OnDisk_EnablesWALAndBusyTimeout locks the concurrency config
// so a regression that drops the DSN pragmas is caught directly, without
// relying on a concurrency test to surface SQLITE_BUSY.
func TestOpen_OnDisk_EnablesWALAndBusyTimeout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pragma.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	var journal string
	if err := s.db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journal); err != nil {
		t.Fatal(err)
	}
	if journal != "wal" {
		t.Errorf("journal_mode = %q, want wal", journal)
	}

	var busy int
	if err := s.db.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busy); err != nil {
		t.Fatal(err)
	}
	if busy <= 0 {
		t.Errorf("busy_timeout = %d, want > 0", busy)
	}
}
