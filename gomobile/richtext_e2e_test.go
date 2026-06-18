package gomobile_test

import (
	"strings"
	"sync"
	"testing"

	"github.com/Deln0r/ygo/gomobile"
)

// textRec implements gomobile.TextChangeListener, accumulating every
// observed delta for assertions.
type textRec struct {
	mu     sync.Mutex
	deltas []string
}

func (r *textRec) OnTextChange(deltaJSON []byte) {
	r.mu.Lock()
	r.deltas = append(r.deltas, string(deltaJSON))
	r.mu.Unlock()
}

func (r *textRec) saw(substr string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, d := range r.deltas {
		if strings.Contains(d, substr) {
			return true
		}
	}
	return false
}

// TestMobile_RichText_RoundTrip drives the full rich-text editor round
// trip: A applies a formatted Quill delta and formats a range; B
// receives the text and observes the formatting in its own change
// deltas — exactly what a native editor binds to.
func TestMobile_RichText_RoundTrip(t *testing.T) {
	url := startServer(t)

	da := gomobile.NewDocWithClientID(81)
	ca := gomobile.NewClient(url, "rich", da)
	if err := ca.Connect(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ca.Close() })

	db := gomobile.NewDocWithClientID(82)
	cb := gomobile.NewClient(url, "rich", db)
	if err := cb.Connect(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cb.Close() })

	waitFor(t, "a synced", ca.Synced)
	waitFor(t, "b synced", cb.Synced)

	tb := db.Text("note")
	rec := &textRec{}
	tb.ObserveChanges(rec)

	// A applies a bold insert via a Quill delta.
	ta := da.Text("note")
	if err := ta.ApplyDelta([]byte(`[{"insert":"Hello","attributes":{"bold":true}}]`)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "b sees Hello", func() bool { return tb.String() == "Hello" })
	waitFor(t, "b observed bold", func() bool { return rec.saw(`"bold"`) })

	// A italicizes the range via Format; B observes that too.
	if err := ta.Format(0, 5, []byte(`{"italic":true}`)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "b observed italic", func() bool { return rec.saw(`"italic"`) })

	// A appends an embed; B sees the document grow by one unit.
	if err := ta.InsertEmbed(5, []byte(`{"image":"x.png"}`)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "b sees embed length", func() bool { return tb.Length() == 6 })
}
