package awareness

import (
	"encoding/hex"
	"strconv"
	"strings"
	"testing"
)

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Clients keep the order their states appeared in, as y-protocols'
// states Map does, and "everyone" encodes in it. A removed client
// leaves the order and comes back at the end; its next state, one
// clock above the one it was removed at, is taken. Bytes from
// y-protocols 1.0.7 for the same history: local 5, then 9, then 2; 9
// removed, then 9 again at clock 2.
func TestClientOrder_AsYProtocols(t *testing.T) {
	a := New(5)
	a.SetLocalState([]byte(`{"n":5}`))
	for _, u := range []string{"010901077b226e223a397d", "010201077b226e223a327d"} {
		if _, err := a.Apply(mustHex(t, u), "x"); err != nil {
			t.Fatal(err)
		}
	}
	if got := a.ClientIDs(); len(got) != 3 || got[0] != 5 || got[1] != 9 || got[2] != 2 {
		t.Fatalf("ClientIDs %v, want [5 9 2]", got)
	}
	const all = "030501077b226e223a357d0901077b226e223a397d0201077b226e223a327d"
	if got := hex.EncodeToString(a.EncodeLive()); got != all {
		t.Fatalf("EncodeLive\n got  %s\n want %s", got, all)
	}
	for i := 0; i < 100; i++ {
		if got := hex.EncodeToString(a.Encode(nil)); got != all {
			t.Fatalf("Encode(nil) run %d: %s, want %s", i, got, all)
		}
	}

	a.RemoveState(9)
	if _, err := a.Apply(mustHex(t, "010902087b226e223a39397d"), "x"); err != nil {
		t.Fatal(err)
	}
	if got := a.ClientIDs(); len(got) != 3 || got[0] != 5 || got[1] != 2 || got[2] != 9 {
		t.Fatalf("after removal and return: ClientIDs %v, want [5 2 9]", got)
	}
	const all2 = "030501077b226e223a357d0201077b226e223a327d0902087b226e223a39397d"
	if got := hex.EncodeToString(a.EncodeLive()); got != all2 {
		t.Fatalf("EncodeLive after the return\n got  %s\n want %s", got, all2)
	}
}

// With only tombstones left, the live snapshot has no entries, while
// Encode(nil) keeps its contract of every known client.
func TestEncodeLive_NoLiveClients(t *testing.T) {
	a := New(5)
	if _, err := a.Apply(mustHex(t, "010901077b226e223a397d"), "x"); err != nil {
		t.Fatal(err)
	}
	a.RemoveState(9)
	if got := hex.EncodeToString(a.EncodeLive()); got != "00" {
		t.Fatalf("EncodeLive %s, want 00", got)
	}
	if got := hex.EncodeToString(a.Encode(nil)); got != "010901046e756c6c" {
		t.Fatalf("Encode(nil) %s, want the tombstone of 9 at its last clock", got)
	}
}

// 'change' fires before 'update', and each list is copied before its
// delivery, as y-protocols does: a change listener added from an update
// listener does not get the current change ("change update").
func TestEvents_ChangeBeforeUpdate(t *testing.T) {
	a := New(5)
	a.SetLocalState([]byte(`{"n":5}`))
	var log []string
	first := true
	a.OnUpdate(func(Summary, any) {
		log = append(log, "update")
		if first {
			first = false
			a.OnChange(func(Summary, any) { log = append(log, "late-change") })
		}
	})
	a.OnChange(func(Summary, any) { log = append(log, "change") })
	a.SetLocalState([]byte(`{"n":6}`))
	if got := strings.Join(log, " "); got != "change update" {
		t.Fatalf("events %q, want \"change update\" (y-protocols)", got)
	}
}

// Listeners run in registration order.
func TestEvents_RegistrationOrder(t *testing.T) {
	a := New(5)
	var log []byte
	for i := 0; i < 20; i++ {
		c := byte('a' + i)
		a.OnUpdate(func(Summary, any) { log = append(log, c) })
	}
	a.SetLocalState([]byte(`{}`))
	if got := string(log); got != "abcdefghijklmnopqrst" {
		t.Fatalf("listeners ran as %q, want registration order", got)
	}
}

// The classes OnChange and OnUpdate report are y-protocols': added for a
// client never seen, updated for one seen before (a revival included),
// removed for a null from one seen before (also again, and for the local
// client's self-eviction, whose state stays), and 'change' only when the
// state differs. Every row is y-protocols 1.0.7's output for the same
// bytes, applied in order to an awareness of client 5 with a state set.
func TestEvents_ClassesAsYProtocols(t *testing.T) {
	r := New(5)
	r.SetLocalState([]byte(`{"me":1}`))
	var log []string
	format := func(kind string, s Summary) string {
		var parts []string
		for _, c := range []struct {
			name string
			ids  []uint64
		}{{"added", s.Added}, {"updated", s.Updated}, {"removed", s.Removed}} {
			if len(c.ids) > 0 {
				var ids []string
				for _, id := range c.ids {
					ids = append(ids, strconv.FormatUint(id, 10))
				}
				parts = append(parts, c.name+":"+strings.Join(ids, ","))
			}
		}
		return kind + " " + strings.Join(parts, " ")
	}
	r.OnChange(func(s Summary, _ any) { log = append(log, format("change", s)) })
	r.OnUpdate(func(s Summary, _ any) { log = append(log, format("update", s)) })
	for _, row := range []struct{ name, bytes, want string }{
		{"arrival", "010901077b2270223a317d", "change added:9 | update added:9"},
		{"changed state", "010902077b2270223a327d", "change updated:9 | update updated:9"},
		{"heartbeat", "010903077b2270223a327d", "update updated:9"},
		{"removal", "010904046e756c6c", "change removed:9 | update removed:9"},
		{"revival", "010905077b2270223a337d", "change updated:9 | update updated:9"},
		{"removal again", "010906046e756c6c", "change removed:9 | update removed:9"},
		{"null while already removed", "010907046e756c6c", "change removed:9 | update removed:9"},
		{"self-eviction", "010501046e756c6c", "change removed:5 | update removed:5"},
	} {
		log = nil
		if _, err := r.Apply(mustHex(t, row.bytes), "x"); err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(log, " | "); got != row.want {
			t.Errorf("%s: %q, want %q", row.name, got, row.want)
		}
	}
	if st, ok := r.LocalState(); !ok || string(st) != `{"me":1}` {
		t.Errorf("local state after self-eviction %s (ok %v), want it kept", st, ok)
	}
	if c, _, _ := r.Meta(5); c != 2 {
		t.Errorf("local clock after self-eviction %d, want 2", c)
	}

	l := New(7)
	log = nil
	l.OnChange(func(s Summary, _ any) { log = append(log, format("change", s)) })
	l.OnUpdate(func(s Summary, _ any) { log = append(log, format("update", s)) })
	for _, row := range []struct {
		name string
		do   func()
		want string
	}{
		{"local first removal", l.RemoveLocalState, "change removed:7 | update removed:7"},
		{"local removal again", l.RemoveLocalState, "change removed:7 | update removed:7"},
		{"local set after removal", func() { l.SetLocalState([]byte(`{"x":1}`)) }, "change added:7 | update added:7"},
		{"local same state", func() { l.SetLocalState([]byte(`{"x":1}`)) }, "update updated:7"},
	} {
		log = nil
		row.do()
		if got := strings.Join(log, " | "); got != row.want {
			t.Errorf("%s: %q, want %q", row.name, got, row.want)
		}
	}
	if c, _, _ := l.Meta(7); c != 4 {
		t.Errorf("local clock %d, want 4 (removed at 1 and 2, set at 3 and 4)", c)
	}
}

// EncodeLive takes the clients and their states under one lock: while a
// peer is removed and revived over and over, no snapshot carries its
// tombstone (run with -race).
func TestEncodeLive_ConsistentWhileStatesChange(t *testing.T) {
	a := New(5)
	a.SetLocalState([]byte(`{"n":5}`))
	done := make(chan struct{})
	go func() {
		defer close(done)
		for c := uint32(1); c <= 400; c++ {
			payload := []byte(`{"p":1}`)
			if c%2 == 0 {
				payload = []byte(NullStateJSON)
			}
			if _, err := a.Apply(encodeUpdate([]wireEntry{{ClientID: 9, Clock: c, JSON: payload}}), "x"); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	for {
		select {
		case <-done:
			return
		default:
		}
		entries, _, err := decodeUpdate(a.EncodeLive())
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if string(e.JSON) == NullStateJSON {
				t.Fatalf("a live snapshot carries the tombstone of %d", e.ClientID)
			}
		}
	}
}

// A state is the same when its JSON value is, as y-protocols compares
// parsed states with equalityDeep: another key order or 1.0 for 1 fires
// OnUpdate but not OnChange, for remote and local states (y-protocols
// 1.0.7: "update" each time).
func TestEvents_SameStateInAnotherSpelling(t *testing.T) {
	r := New(5)
	var log []string
	r.OnChange(func(Summary, any) { log = append(log, "change") })
	r.OnUpdate(func(Summary, any) { log = append(log, "update") })
	msg := func(clock uint32, js string) []byte {
		return encodeUpdate([]wireEntry{{ClientID: 9, Clock: clock, JSON: []byte(js)}})
	}
	if _, err := r.Apply(msg(1, `{"a":1,"b":2}`), "x"); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		clock uint32
		js    string
	}{{2, `{"b":2,"a":1}`}, {3, `{"b":2,"a":1.0}`}} {
		log = nil
		if _, err := r.Apply(msg(row.clock, row.js), "x"); err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(log, " "); got != "update" {
			t.Errorf("remote %s: %q, want \"update\"", row.js, got)
		}
	}
	r.SetLocalState([]byte(`{"x":1,"y":2}`))
	log = nil
	r.SetLocalState([]byte(`{"y":2,"x":1}`))
	if got := strings.Join(log, " "); got != "update" {
		t.Errorf("local in another key order: %q, want \"update\"", got)
	}
}
