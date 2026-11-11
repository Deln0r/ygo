package doc

import (
	"testing"
)

func TestNewDoc_ClientIDIsNonZero(t *testing.T) {
	for i := 0; i < 100; i++ {
		d := NewDoc()
		if d.ClientID() == 0 {
			t.Fatal("ClientID must never be zero (collision with yrs NonZeroU64 reserved sentinel)")
		}
	}
}

func TestNewDoc_ClientIDFitsIn53Bits(t *testing.T) {
	for i := 0; i < 100; i++ {
		d := NewDoc()
		if d.ClientID() > MaxClientID {
			t.Errorf("ClientID %d exceeds MaxClientID %d (would not round-trip through JS safe integer)", d.ClientID(), MaxClientID)
		}
	}
}

func TestNewDoc_DistinctClientIDs(t *testing.T) {
	// Probabilistic: 100 fresh docs should produce 100 distinct
	// 32-bit IDs. The collision probability for 100 draws from a
	// 2^32 space is ~1.2e-6, far below test-flake threshold.
	const n = 100
	seen := make(map[uint64]struct{}, n)
	for i := 0; i < n; i++ {
		id := NewDoc().ClientID()
		if _, dup := seen[id]; dup {
			t.Fatalf("ClientID collision after %d docs (id=%d). Either crypto/rand is broken or the test is unlucky beyond reason.", i, id)
		}
		seen[id] = struct{}{}
	}
}

func TestNewDocWithOptions_ExplicitClientID(t *testing.T) {
	d := NewDocWithOptions(Options{ClientID: 42})
	if d.ClientID() != 42 {
		t.Errorf("ClientID=%d want 42", d.ClientID())
	}
	if !d.GC() {
		t.Error("GC defaulted off; zero-value Options must keep GC enabled")
	}
}

func TestNewDoc_GCDefaultsTrue(t *testing.T) {
	if !NewDoc().GC() {
		t.Error("Default GC must be true to match JS Yjs / yrs")
	}
}

func TestNewDocWithOptions_DisableGC(t *testing.T) {
	d := NewDocWithOptions(Options{ClientID: 1, DisableGC: true})
	if d.GC() {
		t.Error("DisableGC=true did not turn GC off")
	}
}

func TestNewDocWithOptions_PartialOptionsKeepsGC(t *testing.T) {
	// Regression for the previous bug: Options{ClientID: 7} with no
	// DisableGC override must keep GC enabled, not silently disable
	// it via a !=Options{} test.
	d := NewDocWithOptions(Options{ClientID: 7})
	if !d.GC() {
		t.Error("Partial Options silently disabled GC; the zero-value GC default must hold")
	}
}
