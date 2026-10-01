package gomobile_test

import (
	"bytes"
	"sync"
	"testing"

	"github.com/Deln0r/ygo/gomobile"
)

// peerUpdate returns a V1 update from another replica that appends s to
// the text "body" of a document holding base.
func peerUpdate(t *testing.T, base []byte, s string) []byte {
	t.Helper()
	peer := gomobile.NewDocWithClientID(99)
	if err := peer.ApplyUpdate(base); err != nil {
		t.Fatal(err)
	}
	sv := peer.EncodeStateVector()
	body := peer.Text("body")
	if err := body.InsertAt(body.Length(), s); err != nil {
		t.Fatal(err)
	}
	u, err := peer.EncodeDiff(sv)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// The mobile scoped-undo setup: edits through a WithOrigin handle are
// undoable, a collaborator's update applied with its own origin is not.
func TestMobile_UndoTracksTheHandleOrigin(t *testing.T) {
	doc := gomobile.NewDocWithClientID(1)
	local := doc.WithOrigin("local")
	body := local.Text("body")
	um := local.NewTextUndoManager("body")
	defer um.Close()

	if err := body.InsertAt(0, "mine"); err != nil {
		t.Fatal(err)
	}
	um.StopCapturing()
	if err := doc.ApplyUpdateWithOrigin(peerUpdate(t, doc.EncodeStateAsUpdate(), "+peer"), "remote"); err != nil {
		t.Fatal(err)
	}
	if got := body.String(); got != "mine+peer" {
		t.Fatalf("before undo: %q", got)
	}
	if !um.Undo() {
		t.Fatal("undo of the local edit did nothing")
	}
	if got := body.String(); got != "+peer" {
		t.Fatalf("after undo: %q, want the peer's text kept", got)
	}
	if um.CanUndo() {
		t.Fatal("the peer's update is on the undo stack")
	}

	// A plain-handle edit carries no origin: not this manager's either,
	// until it tracks "".
	if err := doc.Text("body").InsertAt(0, "x"); err != nil {
		t.Fatal(err)
	}
	if um.CanUndo() {
		t.Fatal("an edit with no origin was captured by a manager tracking \"local\"")
	}
	um.AddTrackedOrigin("")
	if err := doc.Text("body").InsertAt(0, "y"); err != nil {
		t.Fatal(err)
	}
	if !um.CanUndo() {
		t.Fatal("AddTrackedOrigin(\"\") did not track edits with no origin")
	}
	um.RemoveTrackedOrigin("")
	um.RemoveTrackedOrigin("local")
	if err := body.InsertAt(0, "z"); err != nil {
		t.Fatal(err)
	}
	um.StopCapturing()
	if !um.Undo() || body.String() != "zx+peer" {
		t.Fatalf("after removing both origins only the earlier step should undo; text %q", body.String())
	}
}

// The plain handle keeps the default: no origin is tracked, so a plain
// ApplyUpdate is captured exactly as yjs captures applyUpdate without an
// origin, and ApplyUpdateWithOrigin is how to keep a peer out.
func TestMobile_PlainHandleDefault(t *testing.T) {
	doc := gomobile.NewDocWithClientID(1)
	um := doc.NewTextUndoManager("body")
	defer um.Close()
	if err := doc.ApplyUpdate(peerUpdate(t, doc.EncodeStateAsUpdate(), "a")); err != nil {
		t.Fatal(err)
	}
	if !um.CanUndo() {
		t.Fatal("plain ApplyUpdate should be captured by the default manager")
	}
	um.Undo()
	um.StopCapturing()
	um2 := doc.NewTextUndoManager("body")
	defer um2.Close()
	if err := doc.ApplyUpdateWithOrigin(peerUpdate(t, doc.EncodeStateAsUpdate(), "b"), "remote"); err != nil {
		t.Fatal(err)
	}
	if um2.CanUndo() {
		t.Fatal("an update applied with an origin was captured")
	}
}

type updateLog struct {
	mu      sync.Mutex
	origins []string
	updates [][]byte
}

func (l *updateLog) OnUpdate(update []byte, origin string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.origins = append(l.origins, origin)
	l.updates = append(l.updates, append([]byte(nil), update...))
}

// ObserveUpdates delivers each transaction's update with its string
// origin; replaying them on a fresh replica rebuilds the document. One
// listener per document, replaced through any handle.
func TestMobile_ObserveUpdates(t *testing.T) {
	doc := gomobile.NewDocWithClientID(1)
	local := doc.WithOrigin("local")
	log := &updateLog{}
	local.ObserveUpdates(log)
	if err := local.Text("body").InsertAt(0, "ab"); err != nil {
		t.Fatal(err)
	}
	if err := doc.Text("body").InsertAt(2, "c"); err != nil {
		t.Fatal(err)
	}
	if err := doc.ApplyUpdateWithOrigin(peerUpdate(t, doc.EncodeStateAsUpdate(), "d"), "remote"); err != nil {
		t.Fatal(err)
	}
	want := []string{"local", "", "remote"}
	if len(log.origins) != len(want) {
		t.Fatalf("origins %q, want %q", log.origins, want)
	}
	for i := range want {
		if log.origins[i] != want[i] {
			t.Fatalf("origins %q, want %q", log.origins, want)
		}
	}
	replica := gomobile.NewDocWithClientID(2)
	for _, u := range log.updates {
		if err := replica.ApplyUpdate(u); err != nil {
			t.Fatal(err)
		}
	}
	if got := replica.Text("body").String(); got != "abcd" {
		t.Fatalf("replayed updates give %q", got)
	}

	second := &updateLog{}
	doc.ObserveUpdates(second) // replaces the listener set through local
	if err := doc.Text("body").InsertAt(0, "e"); err != nil {
		t.Fatal(err)
	}
	doc.ObserveUpdates(nil)
	if err := doc.Text("body").InsertAt(0, "f"); err != nil {
		t.Fatal(err)
	}
	if len(log.updates) != 3 || len(second.updates) != 1 {
		t.Fatalf("first listener got %d updates (want 3), second %d (want 1)", len(log.updates), len(second.updates))
	}
}

// Two handles with different origins, edited from different goroutines,
// never swap labels: the origin belongs to the handle, not the document.
func TestMobile_HandlesKeepTheirOrigins(t *testing.T) {
	doc := gomobile.NewDocWithClientID(1)
	log := &updateLog{}
	doc.ObserveUpdates(log)
	var wg sync.WaitGroup
	for _, origin := range []string{"ui", "import"} {
		h := doc.WithOrigin(origin)
		wg.Add(1)
		go func() {
			defer wg.Done()
			m := h.Map("m")
			for i := 0; i < 200; i++ {
				if err := m.SetJSON(origin, []byte(`"`+origin+`"`)); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	counts := map[string]int{}
	for i, o := range log.origins {
		counts[o]++
		// Each handle writes its own origin as the key: the update must
		// carry that key and not the other handle's.
		other := map[string]string{"ui": "import", "import": "ui"}[o]
		if !bytes.Contains(log.updates[i], []byte(o)) || bytes.Contains(log.updates[i], []byte(other)) {
			t.Fatalf("update %d labelled %q does not hold that handle's write", i, o)
		}
	}
	if counts["ui"] != 200 || counts["import"] != 200 || len(counts) != 2 {
		t.Fatalf("origin counts %v, want 200 each", counts)
	}
}

type countingListener struct {
	mu sync.Mutex
	n  int
}

func (c *countingListener) OnUpdate([]byte, string) {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
}

// Replacing the listener while another goroutine commits hands every
// update to exactly one listener: none is lost in the swap.
func TestMobile_ReplaceListenerUnderLoad(t *testing.T) {
	doc := gomobile.NewDocWithClientID(1)
	a, b := &countingListener{}, &countingListener{}
	doc.ObserveUpdates(a)
	const commits = 2000
	done := make(chan struct{})
	go func() {
		defer close(done)
		body := doc.Text("body")
		for i := 0; i < commits; i++ {
			if err := body.InsertAt(0, "x"); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	for i := 0; ; i++ {
		select {
		case <-done:
			a.mu.Lock()
			b.mu.Lock()
			total := a.n + b.n
			b.mu.Unlock()
			a.mu.Unlock()
			if total != commits {
				t.Fatalf("listeners received %d updates for %d commits", total, commits)
			}
			return
		default:
		}
		if i%2 == 0 {
			doc.ObserveUpdates(b)
		} else {
			doc.WithOrigin("other").ObserveUpdates(a)
		}
	}
}
