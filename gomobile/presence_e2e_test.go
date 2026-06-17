package gomobile_test

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/Deln0r/ygo/gomobile"
)

// presenceRec implements gomobile.PresenceListener, keeping the latest
// snapshot for assertions.
type presenceRec struct {
	mu     sync.Mutex
	latest []byte
}

func (p *presenceRec) OnPresenceChange(statesJSON []byte) {
	p.mu.Lock()
	p.latest = append([]byte(nil), statesJSON...)
	p.mu.Unlock()
}

func (p *presenceRec) snapshot() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.latest
}

// TestMobile_Presence_CursorEndToEnd drives the full presence flow a
// Swift / Kotlin app uses to render collaborators' cursors: A publishes
// its name and an encoded cursor as awareness state, B observes the
// presence snapshot, decodes A's cursor, and resolves it to a local
// index. Then A leaves and drops out of B's snapshot.
func TestMobile_Presence_CursorEndToEnd(t *testing.T) {
	url := startServer(t)

	da := gomobile.NewDocWithClientID(71)
	ca := gomobile.NewClient(url, "room", da)
	if err := ca.Connect(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ca.Close() })

	db := gomobile.NewDocWithClientID(72)
	cb := gomobile.NewClient(url, "room", db)
	rec := &presenceRec{}
	cb.ObservePresence(rec) // registered before Connect
	if err := cb.Connect(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cb.Close() })

	waitFor(t, "a synced", ca.Synced)
	waitFor(t, "b synced", cb.Synced)

	// A types so both share a Text for cursor resolution.
	ta := da.Text("note")
	if err := ta.InsertAt(0, "hello"); err != nil {
		t.Fatal(err)
	}
	tb := db.Text("note")
	waitFor(t, "b sees hello", func() bool { return tb.String() == "hello" })

	// A publishes presence: a name plus a cursor anchored at end of "hello".
	cur, err := ta.EncodeCursor(5, 0)
	if err != nil {
		t.Fatal(err)
	}
	state, err := json.Marshal(map[string]any{
		"name":   "ian",
		"cursor": base64.StdEncoding.EncodeToString(cur),
	})
	if err != nil {
		t.Fatal(err)
	}
	ca.SetAwarenessState(state)

	// B's presence listener fires with A's entry.
	waitFor(t, "b sees a's presence", func() bool {
		return strings.Contains(string(rec.snapshot()), `"71"`)
	})

	var states map[string]struct {
		Name   string `json:"name"`
		Cursor string `json:"cursor"`
	}
	if err := json.Unmarshal(rec.snapshot(), &states); err != nil {
		t.Fatalf("parse snapshot: %v", err)
	}
	a, ok := states["71"]
	if !ok {
		t.Fatal("A's presence entry missing")
	}
	if a.Name != "ian" {
		t.Errorf("name = %q, want ian", a.Name)
	}
	curBytes, err := base64.StdEncoding.DecodeString(a.Cursor)
	if err != nil {
		t.Fatalf("decode cursor: %v", err)
	}
	if got := tb.ResolveCursor(curBytes); got != 5 {
		t.Errorf("B resolves A's cursor to %d, want 5", got)
	}

	// PresenceStates returns the same snapshot on demand.
	if !strings.Contains(string(cb.PresenceStates()), `"71"`) {
		t.Error("PresenceStates missing A's entry")
	}

	// A leaves; B's snapshot drops A.
	ca.RemoveAwarenessState()
	waitFor(t, "b sees a leave", func() bool {
		var s map[string]json.RawMessage
		_ = json.Unmarshal(rec.snapshot(), &s)
		_, present := s["71"]
		return !present
	})
}
