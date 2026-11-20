package sqlite_test

import (
	"testing"

	"github.com/Deln0r/ygo/internal/persisttest"
	"github.com/Deln0r/ygo/persist"
	"github.com/Deln0r/ygo/persist/sqlite"
)

// TestStore_Conformance runs the store conformance suite, and the persist
// helpers with it, on SQLite, the suite the core's in-memory store passes.
func TestStore_Conformance(t *testing.T) {
	persisttest.Run(t, func(t *testing.T) persist.VersionedStore {
		s, err := sqlite.Open(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		return s
	})
}
