package awareness

import (
	"testing"
	"time"
)

// RenewLocalState bumps the clock of a state at least olderThan old, fires
// OnUpdate and not OnChange, and leaves a younger, removed or never-set
// state alone.
func TestRenewLocalState(t *testing.T) {
	clock, now := fixedClock(time.Unix(1000, 0))
	a := NewWithClock(7, clock)
	var updates, changes int
	a.OnUpdate(func(s Summary, _ any) {
		if len(s.Updated) == 1 && s.Updated[0] == 7 {
			updates++
		}
	})
	a.OnChange(func(Summary, any) { changes++ })

	if a.RenewLocalState(0) {
		t.Fatal("renewed a state that was never set")
	}
	a.SetLocalState([]byte(`{"n":1}`))
	updates, changes = 0, 0

	*now = now.Add(14 * time.Second)
	if a.RenewLocalState(15 * time.Second) {
		t.Fatal("renewed a state younger than olderThan")
	}
	*now = now.Add(time.Second)
	if !a.RenewLocalState(15 * time.Second) {
		t.Fatal("did not renew a state olderThan old")
	}
	c, last, _ := a.Meta(7)
	if c != 2 || !last.Equal(*now) {
		t.Errorf("after renewal clock %d, lastUpdated %v; want 2, %v", c, last, *now)
	}
	if updates != 1 || changes != 0 {
		t.Errorf("renewal fired %d updates and %d changes, want 1 and 0", updates, changes)
	}
	if s, _ := a.LocalState(); string(s) != `{"n":1}` {
		t.Errorf("state after renewal %s", s)
	}
	if a.RenewLocalState(time.Second) {
		t.Error("renewed again right after a renewal")
	}

	a.RemoveLocalState()
	*now = now.Add(time.Minute)
	if a.RenewLocalState(0) {
		t.Error("renewal revived a removed state")
	}
	if _, ok := a.LocalState(); ok {
		t.Error("local state present after removal")
	}
}

// ForgetRemote drops every remote client, live or tombstoned, keeps the
// local one, reports the live ones removed, and takes a remote state again
// at the clock it had before.
func TestForgetRemote(t *testing.T) {
	a := New(1)
	a.SetLocalState([]byte(`{"me":1}`))
	peer := New(2)
	peer.SetLocalState([]byte(`{"p":2}`))
	gone := New(3)
	gone.SetLocalState([]byte(`{"p":3}`))
	gone.RemoveLocalState()
	for _, u := range [][]byte{peer.Encode(nil), gone.Encode(nil)} {
		if _, err := a.Apply(u, "remote"); err != nil {
			t.Fatal(err)
		}
	}
	var reported, changed []uint64
	a.OnUpdate(func(s Summary, _ any) { reported = append(reported, s.Removed...) })
	a.OnChange(func(s Summary, _ any) { changed = append(changed, s.Removed...) })

	got := a.ForgetRemote()
	if len(got) != 1 || got[0] != 2 || len(reported) != 1 || reported[0] != 2 || len(changed) != 1 || changed[0] != 2 {
		t.Errorf("ForgetRemote returned %v, OnUpdate removed %v, OnChange removed %v; want [2] for all", got, reported, changed)
	}
	if ids := a.ClientIDs(); len(ids) != 1 || ids[0] != 1 {
		t.Errorf("clients after ForgetRemote %v, want [1]", ids)
	}
	for _, id := range []uint64{2, 3} {
		if _, _, ok := a.Meta(id); ok {
			t.Errorf("client %d still tracked", id)
		}
	}
	if s, _ := a.LocalState(); string(s) != `{"me":1}` {
		t.Errorf("local state after ForgetRemote %s", s)
	}
	if again := a.ForgetRemote(); again != nil {
		t.Errorf("second ForgetRemote returned %v", again)
	}

	// The same bytes, at the clock seen before, are taken again.
	if _, err := a.Apply(peer.Encode(nil), "remote"); err != nil {
		t.Fatal(err)
	}
	if string(a.States()[2]) != `{"p":2}` {
		t.Error("peer's state at its old clock refused after ForgetRemote")
	}
}
