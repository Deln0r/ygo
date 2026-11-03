package awareness

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fixedClock returns a controllable time source for tests.
func fixedClock(start time.Time) (func() time.Time, *time.Time) {
	now := start
	return func() time.Time { return now }, &now
}

func TestNew_ClientID(t *testing.T) {
	a := New(42)
	if a.ClientID() != 42 {
		t.Errorf("ClientID = %d, want 42", a.ClientID())
	}
}

// The first state goes out with clock 1, as y-protocols' does after its
// implicit {} at 0: a receiver that has not seen the client takes only
// a clock above 0. Every later set bumps it, an equal one too.
func TestSetLocalState_ClockStartsAtOneBumpsOnEachCall(t *testing.T) {
	a := New(1)
	a.SetLocalState([]byte(`{"cursor":1}`))
	c, _, ok := a.Meta(1)
	if !ok || c != 1 {
		t.Errorf("after first set: clock=%d ok=%v, want 1 true", c, ok)
	}
	a.SetLocalState([]byte(`{"cursor":2}`))
	c, _, _ = a.Meta(1)
	if c != 2 {
		t.Errorf("after second set: clock=%d, want 2", c)
	}
	a.SetLocalState([]byte(`{"cursor":2}`)) // same value — heartbeat
	c, _, _ = a.Meta(1)
	if c != 3 {
		t.Errorf("after no-op heartbeat set: clock=%d, want 3 (clock must bump even on equal state)", c)
	}
}

func TestSetLocalState_LocalStateRoundTrip(t *testing.T) {
	a := New(1)
	state := []byte(`{"name":"Alice","color":"#FFAA00"}`)
	a.SetLocalState(state)
	got, ok := a.LocalState()
	if !ok || !bytes.Equal(got, state) {
		t.Errorf("LocalState = %s, want %s", got, state)
	}
	// Mutating returned bytes must NOT affect Awareness state.
	got[0] = 'X'
	got2, _ := a.LocalState()
	if got2[0] == 'X' {
		t.Error("LocalState returned a shared buffer; expected a copy")
	}
}

func TestSetLocalState_NilArgRemoves(t *testing.T) {
	a := New(1)
	a.SetLocalState([]byte(`{"x":1}`))
	a.SetLocalState(nil)
	if _, ok := a.LocalState(); ok {
		t.Error("nil SetLocalState should remove; LocalState still reports present")
	}
}

func TestRemoveLocalState_KeepsClockBumpsAndFiresEvent(t *testing.T) {
	a := New(1)
	a.SetLocalState([]byte(`{"x":1}`))
	a.SetLocalState([]byte(`{"x":2}`)) // clock = 2

	var removed []uint64
	a.OnUpdate(func(s Summary, _ any) { removed = append(removed, s.Removed...) })

	a.RemoveLocalState()
	if _, ok := a.LocalState(); ok {
		t.Error("LocalState present after RemoveLocalState")
	}
	c, _, ok := a.Meta(1)
	if !ok {
		t.Fatal("Meta missing after remove — entry must be retained for clock survival")
	}
	if c != 3 {
		t.Errorf("clock after remove = %d, want 3 (clock must bump)", c)
	}
	if len(removed) != 1 || removed[0] != 1 {
		t.Errorf("OnUpdate Removed = %v, want [1]", removed)
	}
}

// Removing the local state before any was set is y-protocols'
// setLocalState(null) after its implicit {}: a tombstone at clock 1,
// reported removed, which clears a state a peer still holds for this
// client ID; the next state goes out at clock 2.
func TestRemoveLocalState_BeforeAnySet(t *testing.T) {
	a := New(1)
	var removed []uint64
	a.OnUpdate(func(s Summary, _ any) { removed = append(removed, s.Removed...) })
	a.RemoveLocalState()
	if c, _, ok := a.Meta(1); !ok || c != 1 {
		t.Fatalf("clock after the first removal %d (ok %v), want 1", c, ok)
	}
	if len(removed) != 1 || removed[0] != 1 {
		t.Fatalf("OnUpdate Removed = %v, want [1]", removed)
	}
	if got := a.Encode(nil); string(got) != "\x01\x01\x01\x04null" {
		t.Fatalf("Encode(nil) = %x, want the tombstone of 1 at clock 1", got)
	}
	a.SetLocalState([]byte(`{}`))
	if c, _, _ := a.Meta(1); c != 2 {
		t.Fatalf("clock of the next state %d, want 2", c)
	}
}

func TestStates_ExcludesRemovedEntriesReturnsDeepCopy(t *testing.T) {
	a := New(1)
	a.SetLocalState([]byte(`{"x":1}`))

	// Inject a remote client manually via Apply to test cross-client States().
	remote := New(2)
	remote.SetLocalState([]byte(`{"y":2}`))
	if _, err := a.Apply(remote.Encode(nil), "remote"); err != nil {
		t.Fatal(err)
	}

	got := a.States()
	if len(got) != 2 {
		t.Errorf("len=%d, want 2", len(got))
	}

	// Remove client 2 via wire and confirm States() drops it.
	remote.RemoveLocalState()
	if _, err := a.Apply(remote.Encode([]uint64{2}), "remote"); err != nil {
		t.Fatal(err)
	}
	got = a.States()
	if len(got) != 1 {
		t.Errorf("after remove: len=%d, want 1", len(got))
	}
	if _, ok := got[2]; ok {
		t.Error("removed client still in States()")
	}

	// Deep-copy: mutating returned bytes does not affect Awareness.
	got[1][0] = 'X'
	again := a.States()
	if again[1][0] == 'X' {
		t.Error("States returned shared buffers; expected copies")
	}
}

func TestOnChange_OnlyFiresOnContentChange(t *testing.T) {
	a := New(1)
	var updateCount, changeCount atomic.Int32
	a.OnUpdate(func(_ Summary, _ any) { updateCount.Add(1) })
	a.OnChange(func(_ Summary, _ any) { changeCount.Add(1) })

	a.SetLocalState([]byte(`{"x":1}`)) // add: update + change
	a.SetLocalState([]byte(`{"x":1}`)) // heartbeat: update only
	a.SetLocalState([]byte(`{"x":2}`)) // content changed: update + change

	if got := updateCount.Load(); got != 3 {
		t.Errorf("update count = %d, want 3", got)
	}
	if got := changeCount.Load(); got != 2 {
		t.Errorf("change count = %d, want 2", got)
	}
}

func TestOnUpdate_UnsubscribeStopsCallbacks(t *testing.T) {
	a := New(1)
	var count atomic.Int32
	unsub := a.OnUpdate(func(_ Summary, _ any) { count.Add(1) })
	a.SetLocalState([]byte(`{"x":1}`))
	unsub()
	a.SetLocalState([]byte(`{"x":2}`))
	if got := count.Load(); got != 1 {
		t.Errorf("callback count = %d, want 1 (second call was after unsubscribe)", got)
	}
}

func TestEncodeDecode_RoundTrip(t *testing.T) {
	a := New(1)
	a.SetLocalState([]byte(`{"a":1}`))

	other := New(2)
	other.SetLocalState([]byte(`{"b":"hello"}`))
	if _, err := a.Apply(other.Encode(nil), "remote"); err != nil {
		t.Fatal(err)
	}

	wire := a.Encode([]uint64{1, 2})

	// Apply to a fresh empty Awareness; structural states must match.
	target := New(99)
	summary, err := target.Apply(wire, "remote")
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Added) != 2 {
		t.Errorf("Added=%v, want 2 added", summary.Added)
	}
	wantStates := a.States()
	gotStates := target.States()
	if len(gotStates) != len(wantStates) {
		t.Fatalf("state count mismatch: got %d, want %d", len(gotStates), len(wantStates))
	}
	for id, payload := range wantStates {
		if got, ok := gotStates[id]; !ok || !bytes.Equal(got, payload) {
			t.Errorf("client %d: got %s, want %s", id, got, payload)
		}
	}
}

func TestEncode_AllByDefault(t *testing.T) {
	a := New(1)
	a.SetLocalState([]byte(`{"x":1}`))
	other := New(2)
	other.SetLocalState([]byte(`{"y":2}`))
	a.Apply(other.Encode(nil), "remote")

	wireAll := a.Encode(nil)
	wireExplicit := a.Encode([]uint64{1, 2})

	// Both should decode to the same set of entries (order may differ).
	gotAll, _, err := decodeUpdate(wireAll)
	if err != nil {
		t.Fatal(err)
	}
	gotExp, _, err := decodeUpdate(wireExplicit)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotAll) != 2 || len(gotExp) != 2 {
		t.Fatalf("counts: nil=%d explicit=%d, want both=2", len(gotAll), len(gotExp))
	}
}

func TestApply_RejectsLowerClock(t *testing.T) {
	a := New(1)
	src := New(2)
	src.SetLocalState([]byte(`{"v":1}`))
	src.SetLocalState([]byte(`{"v":2}`)) // src clock = 1
	a.Apply(src.Encode(nil), "remote")

	// Craft an entry with clock = 0 for client 2 — must be rejected.
	stale := encodeUpdate([]wireEntry{
		{ClientID: 2, Clock: 0, JSON: []byte(`{"v":99}`)},
	})
	summary, err := a.Apply(stale, "remote")
	if err != nil {
		t.Fatal(err)
	}
	if !summary.IsEmpty() {
		t.Errorf("stale apply produced summary %+v, want empty", summary)
	}
	got := a.States()[2]
	if !bytes.Equal(got, []byte(`{"v":2}`)) {
		t.Errorf("state mutated by stale apply: %s", got)
	}
}

func TestApply_TiedClockRemovalWins(t *testing.T) {
	a := New(1)

	// Inject client 2 at clock 5 with state.
	withState := encodeUpdate([]wireEntry{
		{ClientID: 2, Clock: 5, JSON: []byte(`{"v":1}`)},
	})
	a.Apply(withState, "remote")

	// Same clock, but null state — must override per JS:261 / Rust:411.
	removal := encodeUpdate([]wireEntry{
		{ClientID: 2, Clock: 5, JSON: []byte(NullStateJSON)},
	})
	summary, _ := a.Apply(removal, "remote")
	if len(summary.Removed) != 1 || summary.Removed[0] != 2 {
		t.Errorf("removal summary = %+v, want Removed=[2]", summary)
	}
	if _, ok := a.States()[2]; ok {
		t.Error("client 2 still in States after tied-clock removal")
	}
}

func TestApply_TiedClockNonNullDoesNotOverride(t *testing.T) {
	a := New(1)
	first := encodeUpdate([]wireEntry{{ClientID: 2, Clock: 5, JSON: []byte(`{"v":1}`)}})
	a.Apply(first, "remote")

	// Same clock, different content — must NOT override.
	second := encodeUpdate([]wireEntry{{ClientID: 2, Clock: 5, JSON: []byte(`{"v":99}`)}})
	summary, _ := a.Apply(second, "remote")
	if !summary.IsEmpty() {
		t.Errorf("tied-clock non-null overwrote: summary=%+v", summary)
	}
	if got := a.States()[2]; !bytes.Equal(got, []byte(`{"v":1}`)) {
		t.Errorf("state mutated: %s", got)
	}
}

func TestApply_SelfEvictionDefense(t *testing.T) {
	// Local client is 1, online with state. A remote tries to null
	// us with clock 5 — we must bump OUR clock instead of removing.
	a := New(1)
	a.SetLocalState([]byte(`{"local":"online"}`))
	c, _, _ := a.Meta(1)
	_ = c

	evict := encodeUpdate([]wireEntry{{ClientID: 1, Clock: 5, JSON: []byte(NullStateJSON)}})
	a.Apply(evict, "remote")

	state, ok := a.LocalState()
	if !ok {
		t.Fatal("self-eviction succeeded; local state removed (defense failed)")
	}
	if !bytes.Equal(state, []byte(`{"local":"online"}`)) {
		t.Errorf("local state mutated by eviction attempt: %s", state)
	}
	newClock, _, _ := a.Meta(1)
	if newClock != 6 {
		t.Errorf("post-defense clock = %d, want 6 (incoming clock %d + 1)", newClock, 5)
	}
}

func TestApply_StaleReviveAfterRemoveRejected(t *testing.T) {
	a := New(1)
	src := New(2)
	src.SetLocalState([]byte(`{"v":1}`)) // clock 0
	src.SetLocalState([]byte(`{"v":2}`)) // clock 1
	a.Apply(src.Encode(nil), "remote")

	src.RemoveLocalState() // clock 2, data nil
	a.Apply(src.Encode([]uint64{2}), "remote")

	if _, ok := a.States()[2]; ok {
		t.Fatal("client 2 still in States after removal")
	}

	// Now a stale revival at clock 1 arrives — must be rejected,
	// proving the retained clock survives across the removal.
	stale := encodeUpdate([]wireEntry{{ClientID: 2, Clock: 1, JSON: []byte(`{"v":"stale"}`)}})
	summary, _ := a.Apply(stale, "remote")
	if !summary.IsEmpty() {
		t.Errorf("stale revive accepted: summary=%+v", summary)
	}
	if _, ok := a.States()[2]; ok {
		t.Error("stale revive recreated client 2")
	}
}

func TestSweepOutdated_RemovesOldEntriesNeverLocal(t *testing.T) {
	clock, nowPtr := fixedClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	a := NewWithClock(1, clock)
	a.SetLocalState([]byte(`{"v":1}`))

	// Inject two remote clients at the current time.
	remoteA := NewWithClock(10, clock)
	remoteA.SetLocalState([]byte(`{"a":1}`))
	remoteB := NewWithClock(20, clock)
	remoteB.SetLocalState([]byte(`{"b":1}`))

	a.Apply(remoteA.Encode(nil), "remote")
	a.Apply(remoteB.Encode(nil), "remote")

	// Advance clock 60s — both remotes are stale, local is too but must be spared.
	*nowPtr = nowPtr.Add(60 * time.Second)

	removed := a.SweepOutdated(30 * time.Second)
	if len(removed) != 2 || removed[0] != 10 || removed[1] != 20 {
		t.Errorf("removed = %v, want [10 20] (sorted ascending)", removed)
	}

	if _, ok := a.LocalState(); !ok {
		t.Error("local client was swept")
	}
	if _, ok := a.States()[10]; ok {
		t.Error("remote 10 not swept")
	}
}

func TestSweepOutdated_NoOpWhenNothingStale(t *testing.T) {
	clock, _ := fixedClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	a := NewWithClock(1, clock)
	a.SetLocalState([]byte(`{"v":1}`))

	removed := a.SweepOutdated(30 * time.Second)
	if removed != nil {
		t.Errorf("removed = %v, want nil", removed)
	}
}

func TestSetLocalStateJSON_MarshalsAndSets(t *testing.T) {
	a := New(1)
	type S struct {
		Cursor int    `json:"cursor"`
		Name   string `json:"name"`
	}
	if err := a.SetLocalStateJSON(S{Cursor: 42, Name: "Alice"}); err != nil {
		t.Fatal(err)
	}
	raw, ok := a.LocalState()
	if !ok {
		t.Fatal("LocalState not set")
	}
	var decoded S
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Cursor != 42 || decoded.Name != "Alice" {
		t.Errorf("decoded = %+v, want {42 Alice}", decoded)
	}
}

func TestSetLocalStateJSON_NilRemoves(t *testing.T) {
	a := New(1)
	a.SetLocalState([]byte(`{"x":1}`))
	if err := a.SetLocalStateJSON(nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.LocalState(); ok {
		t.Error("nil SetLocalStateJSON did not remove")
	}
}

func TestSetLocalStateJSON_MarshalErrorPreservesState(t *testing.T) {
	a := New(1)
	a.SetLocalState([]byte(`{"x":1}`))
	// A channel cannot be marshalled — json.Marshal returns an error.
	err := a.SetLocalStateJSON(make(chan int))
	if err == nil {
		t.Fatal("expected marshal error")
	}
	got, _ := a.LocalState()
	if !bytes.Equal(got, []byte(`{"x":1}`)) {
		t.Errorf("state mutated on marshal error: %s", got)
	}
}

func TestApply_InvalidBytesReturnsErr(t *testing.T) {
	a := New(1)
	// One byte saying "1 entry" then EOF — decode of clientID fails.
	bad := []byte{0x01}
	summary, err := a.Apply(bad, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if !summary.IsEmpty() {
		t.Errorf("summary not empty on decode error: %+v", summary)
	}
}

func TestRemoveState_RemovesPeerNotLocal(t *testing.T) {
	a := New(1)
	src := New(2)
	src.SetLocalState([]byte(`{"v":1}`))
	a.Apply(src.Encode(nil), "remote")

	a.RemoveState(2)
	if _, ok := a.States()[2]; ok {
		t.Error("client 2 not removed")
	}
	c, _, ok := a.Meta(2)
	if !ok || c == 0 {
		t.Errorf("clock not bumped on RemoveState; got clock=%d ok=%v", c, ok)
	}

	// Local removal via RemoveState should funnel into RemoveLocalState.
	a.SetLocalState([]byte(`{"local":"online"}`))
	a.RemoveState(1)
	if _, ok := a.LocalState(); ok {
		t.Error("RemoveState(localID) did not remove local")
	}
}

func TestWireFixture_KnownBytesDecode(t *testing.T) {
	// Hand-built wire payload: one entry, client=5, clock=10,
	// json=`{"a":1}` (7 bytes).
	//
	//   0x01           varuint 1 entry
	//   0x05           varuint clientID 5
	//   0x0a           varuint clock 10
	//   0x07           varuint string-length 7
	//   { " a " : 1 }  ASCII payload
	wire := []byte{
		0x01,
		0x05,
		0x0a,
		0x07,
		'{', '"', 'a', '"', ':', '1', '}',
	}
	entries, tail, err := decodeUpdate(wire)
	if err != nil {
		t.Fatal(err)
	}
	if len(tail) != 0 {
		t.Errorf("tail not empty: %d bytes", len(tail))
	}
	if len(entries) != 1 {
		t.Fatalf("len=%d, want 1", len(entries))
	}
	e := entries[0]
	if e.ClientID != 5 || e.Clock != 10 || string(e.JSON) != `{"a":1}` {
		t.Errorf("got %+v, want {5 10 {\"a\":1}}", e)
	}

	// Re-encode and confirm bytes round-trip exactly (single-entry,
	// so iteration order is unambiguous).
	roundTrip := encodeUpdate(entries)
	if !bytes.Equal(roundTrip, wire) {
		t.Errorf("round-trip:\n got  %x\n want %x", roundTrip, wire)
	}
}

func TestWireFixture_NullRemovalSentinel(t *testing.T) {
	// Removal entry: client=7, clock=3, json=literal four-byte
	// string "null".
	wire := []byte{
		0x01,
		0x07,
		0x03,
		0x04,
		'n', 'u', 'l', 'l',
	}
	entries, _, err := decodeUpdate(wire)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].ClientID != 7 || string(entries[0].JSON) != NullStateJSON {
		t.Errorf("got %+v, want one entry with NullStateJSON", entries)
	}

	// Apply to a fresh Awareness; since client 7 was never seen,
	// the null-state apply records the client at the given clock
	// without Added/Updated/Removed (creates a tombstone entry).
	a := New(1)
	summary, _ := a.Apply(wire, "remote")
	if !summary.IsEmpty() {
		t.Errorf("null-apply to never-seen client produced summary %+v, want empty", summary)
	}
	c, _, ok := a.Meta(7)
	if !ok || c != 3 {
		t.Errorf("Meta(7) = (%d, _, %v), want (3, _, true)", c, ok)
	}
}

func TestConcurrent_LocalAndRemoteApply(t *testing.T) {
	// Stress: N goroutines bumping local state, M goroutines
	// applying synthetic remote updates. Must not race-detect or
	// corrupt state.
	const localUpdaters = 4
	const remoteAppliers = 4
	const perGoroutine = 50

	a := New(1)
	var wg sync.WaitGroup

	for g := 0; g < localUpdaters; g++ {
		g := g
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				a.SetLocalState([]byte(fmt.Sprintf(`{"g":%d,"i":%d}`, g, i)))
			}
		}()
	}

	for g := 0; g < remoteAppliers; g++ {
		g := g
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				wire := encodeUpdate([]wireEntry{{
					ClientID: uint64(100 + g),
					Clock:    uint32(i),
					JSON:     []byte(fmt.Sprintf(`{"r":%d}`, i)),
				}})
				_, _ = a.Apply(wire, "remote")
			}
		}()
	}
	wg.Wait()

	// Local client survived all the contention; remote clients are present at their final clock.
	if _, ok := a.LocalState(); !ok {
		t.Error("local state missing after contention")
	}
	for g := 0; g < remoteAppliers; g++ {
		c, _, ok := a.Meta(uint64(100 + g))
		if !ok || c != perGoroutine-1 {
			t.Errorf("remote client %d: clock=%d ok=%v, want %d true", 100+g, c, ok, perGoroutine-1)
		}
	}
}

func TestApply_OriginForwardedToSubscribers(t *testing.T) {
	a := New(1)
	var seenOrigins []any
	a.OnUpdate(func(_ Summary, origin any) { seenOrigins = append(seenOrigins, origin) })

	src := New(2)
	src.SetLocalState([]byte(`{"v":1}`))
	a.Apply(src.Encode(nil), "remote-peer-XYZ")
	if len(seenOrigins) != 1 || seenOrigins[0] != "remote-peer-XYZ" {
		t.Errorf("seen origins = %v, want [remote-peer-XYZ]", seenOrigins)
	}
}

func TestSortAscending(t *testing.T) {
	// The internal helper is small; one direct test guards regressions.
	in := []uint64{5, 1, 4, 2, 3}
	sortAscending(in)
	if !sort.SliceIsSorted(in, func(i, j int) bool { return in[i] < in[j] }) {
		t.Errorf("sortAscending result not sorted: %v", in)
	}
}
