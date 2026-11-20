package persist_test

import (
	"testing"

	"github.com/Deln0r/ygo/internal/memstore"
	"github.com/Deln0r/ygo/internal/persisttest"
	"github.com/Deln0r/ygo/persist"
)

// TestStore_Conformance runs the store conformance suite, and the persist
// helpers with it, on the in-memory store; persist/sqlite runs the same
// suite on SQLite.
func TestStore_Conformance(t *testing.T) {
	persisttest.Run(t, func(*testing.T) persist.VersionedStore { return memstore.New() })
}
