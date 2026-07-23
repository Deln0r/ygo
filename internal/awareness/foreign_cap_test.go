package awareness

import (
	"bytes"
	"testing"
	"time"
)

// applyOne fabricates a single-client presence update (state {"v":1}) for id and
// integrates it into a via the foreign path (ApplyForeign) or the local path
// (Apply). It mirrors how the server routes relayed vs. locally-originated
// presence, so these tests exercise the exact admission split.
func applyOne(t *testing.T, a *Awareness, clock func() time.Time, id uint64, foreign bool) {
	t.Helper()
	r := NewWithClock(id, clock)
	r.SetLocalState([]byte(`{"v":1}`))
	var err error
	if foreign {
		_, err = a.ApplyForeign(r.Encode(nil), "backplane")
	} else {
		_, err = a.Apply(r.Encode(nil), "remote")
	}
	if err != nil {
		t.Fatalf("apply id=%d foreign=%v: %v", id, foreign, err)
	}
}

// TestApplyForeign_SubCapReservesLocalFloor is the core partition guarantee: a
// flood of foreign clientIDs fills at most maxForeignClients slots, leaving
// maxClients-maxForeignClients slots that local admissions (plain Apply) can
// still claim. Without the sub-cap the flood would fill the whole cap and deny
// every later local client.
func TestApplyForeign_SubCapReservesLocalFloor(t *testing.T) {
	clock, _ := fixedClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	a := NewWithClock(1, clock)
	a.SetMaxClients(10)
	a.SetMaxForeignClients(6)

	// Flood 100 distinct foreign clientIDs: only 6 are admitted.
	for id := uint64(100); id < 200; id++ {
		applyOne(t, a, clock, id, true)
	}
	if got := len(a.states); got != 6 {
		t.Fatalf("foreign flood filled %d slots, want 6 (the foreign sub-cap)", got)
	}

	// The reserved 4 slots (10-6) remain available to local clients even
	// though the foreign sub-cap is saturated.
	for id := uint64(10); id < 14; id++ {
		applyOne(t, a, clock, id, false)
		if _, ok := a.States()[id]; !ok {
			t.Errorf("local client %d denied a slot despite the reservation", id)
		}
	}
	if got := len(a.states); got != 10 {
		t.Fatalf("states = %d after filling the reservation, want 10 (maxClients)", got)
	}

	// The cap is now full; a further local client is refused.
	applyOne(t, a, clock, 14, false)
	if _, ok := a.States()[14]; ok {
		t.Error("local client 14 admitted past maxClients; want dropped")
	}
}

// TestApplyForeign_DisabledSharesFullCap: with no foreign sub-cap (0), foreign
// presence again shares the whole maxClients budget, the historical behavior.
func TestApplyForeign_DisabledSharesFullCap(t *testing.T) {
	clock, _ := fixedClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	a := NewWithClock(1, clock)
	a.SetMaxClients(5)
	a.SetMaxForeignClients(0) // partition disabled

	for id := uint64(100); id < 200; id++ {
		applyOne(t, a, clock, id, true)
	}
	if got := len(a.states); got != 5 {
		t.Fatalf("foreign flood filled %d slots, want 5 (the full cap, sub-cap disabled)", got)
	}
}

// TestApply_LocalIgnoresForeignSubCap: the foreign sub-cap never gates the local
// admission path — plain Apply keeps admitting up to maxClients regardless.
func TestApply_LocalIgnoresForeignSubCap(t *testing.T) {
	clock, _ := fixedClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	a := NewWithClock(1, clock)
	a.SetMaxClients(10)
	a.SetMaxForeignClients(3)

	for id := uint64(100); id < 200; id++ {
		applyOne(t, a, clock, id, false) // all via the local path
	}
	if got := len(a.states); got != 10 {
		t.Fatalf("local admissions filled %d slots, want 10 (foreign sub-cap must not gate them)", got)
	}
}

// TestApplyForeign_ExistingForeignKeepsUpdating: the sub-cap only gates
// brand-new clientIDs; an already-admitted foreign client keeps updating after
// the sub-cap is saturated.
func TestApplyForeign_ExistingForeignKeepsUpdating(t *testing.T) {
	clock, _ := fixedClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	a := NewWithClock(1, clock)
	a.SetMaxClients(10)
	a.SetMaxForeignClients(2)

	first := NewWithClock(100, clock)
	first.SetLocalState([]byte(`{"v":"a"}`))
	if _, err := a.ApplyForeign(first.Encode(nil), "backplane"); err != nil {
		t.Fatal(err)
	}
	applyOne(t, a, clock, 101, true) // sub-cap now saturated at 2
	applyOne(t, a, clock, 102, true) // dropped
	if _, ok := a.States()[102]; ok {
		t.Fatal("foreign 102 admitted past the sub-cap; want dropped")
	}

	// The already-tracked foreign client 100 still updates.
	first.SetLocalState([]byte(`{"v":"b"}`))
	if _, err := a.ApplyForeign(first.Encode(nil), "backplane"); err != nil {
		t.Fatal(err)
	}
	if got := a.States()[100]; !bytes.Equal(got, []byte(`{"v":"b"}`)) {
		t.Errorf("existing foreign client update blocked: states[100] = %s", got)
	}
}
