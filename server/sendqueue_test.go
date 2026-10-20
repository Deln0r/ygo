package server

import (
	"bytes"
	"errors"
	"math"
	"testing"
	"time"
)

// fakeClock is a settable clock for the queue's stall check.
type fakeClock struct{ t time.Time }

func (f *fakeClock) now() time.Time { return f.t }

func newTestQueue(maxLen int, maxBytes int64) (*sendQueue, *fakeClock) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	q := newSendQueue(maxLen, maxBytes, time.Second)
	q.now = clk.now
	return q, clk
}

func mustTake(t *testing.T, q *sendQueue) sendItem {
	t.Helper()
	it, cause := q.take()
	if cause != causeNone {
		t.Fatalf("take: cause %d, want an envelope", cause)
	}
	return it
}

// Envelopes come out in the order they went in, replies and fan-out alike.
func TestSendQueue_FIFO(t *testing.T) {
	q, _ := newTestQueue(-1, -1)
	q.pushFanout([]byte("a"))
	q.pushFanout([]byte("b"))
	if _, err := q.pushReply([]byte("c")); err != nil {
		t.Fatal(err)
	}
	q.pushFanout([]byte("d"))
	for _, want := range []string{"a", "b", "c", "d"} {
		if got := mustTake(t, q).env; !bytes.Equal(got, []byte(want)) {
			t.Fatalf("took %q, want %q", got, want)
		}
	}
}

// The length bound alone: an append that finds maxLen fan-out envelopes
// queued overflows, once; later appends are dropped and not counted again.
func TestSendQueue_LengthBound(t *testing.T) {
	q, _ := newTestQueue(2, -1)
	if q.pushFanout([]byte("1")) || q.pushFanout([]byte("2")) {
		t.Fatal("overflowed below the bound")
	}
	if !q.pushFanout([]byte("3")) {
		t.Fatal("append at the bound did not overflow")
	}
	if got := q.terminalCause(); got != causeQueueFull {
		t.Fatalf("cause %d, want causeQueueFull", got)
	}
	if n, b := q.backlog(); n != 0 || b != 0 {
		t.Fatalf("backlog after overflow = %d envelopes, %d bytes; want it dropped", n, b)
	}
	if q.pushFanout([]byte("4")) {
		t.Fatal("an append after the overflow was counted as another overflow")
	}
	if _, cause := q.take(); cause != causeQueueFull {
		t.Fatalf("take after overflow: cause %d, want causeQueueFull", cause)
	}
}

// The byte bound alone, checked before the append: equality overflows, and
// one envelope is never refused for its own size.
func TestSendQueue_ByteBound(t *testing.T) {
	q, _ := newTestQueue(-1, 10)
	if q.pushFanout(make([]byte, 100)) {
		t.Fatal("one envelope larger than the bound was refused by an empty queue")
	}
	if !q.pushFanout([]byte("x")) {
		t.Fatal("an append behind 100 queued bytes did not overflow a 10-byte bound")
	}

	q, _ = newTestQueue(-1, 10)
	if q.pushFanout(make([]byte, 6)) || q.pushFanout(make([]byte, 4)) {
		t.Fatal("overflowed below the bound")
	}
	if !q.pushFanout([]byte("x")) { // exactly 10 bytes queued
		t.Fatal("an append at exactly the byte bound did not overflow")
	}

	q, _ = newTestQueue(-1, 10)
	if q.pushFanout(make([]byte, 9)) || q.pushFanout(make([]byte, 50)) {
		t.Fatal("an append below the bound overflowed")
	}
}

// Disabled bounds never overflow.
func TestSendQueue_DisabledBounds(t *testing.T) {
	q, _ := newTestQueue(-1, -1)
	for i := 0; i < 10000; i++ {
		if q.pushFanout(make([]byte, 1024)) {
			t.Fatalf("overflowed at %d with both bounds disabled", i)
		}
	}
}

// Taking an envelope frees its place under both bounds.
func TestSendQueue_TakeFreesBound(t *testing.T) {
	q, _ := newTestQueue(1, 4)
	q.pushFanout([]byte("abcd"))
	mustTake(t, q)
	if q.pushFanout([]byte("efgh")) {
		t.Fatal("overflowed after the only queued envelope was taken")
	}
	if n, b := q.backlog(); n != 1 || b != 4 {
		t.Fatalf("backlog = %d, %d; want 1, 4", n, b)
	}
}

// Replies are not counted against either bound, but show in the backlog.
func TestSendQueue_RepliesNotCounted(t *testing.T) {
	q, _ := newTestQueue(1, 1)
	if _, err := q.pushReply(make([]byte, 1000)); err != nil {
		t.Fatal(err)
	}
	if q.pushFanout([]byte("a")) {
		t.Fatal("a queued reply counted against the bounds")
	}
	if n, b := q.backlog(); n != 2 || b != 1001 {
		t.Fatalf("backlog = %d, %d; want 2, 1001", n, b)
	}
	if !q.pushFanout([]byte("b")) {
		t.Fatal("the fan-out bound did not hold with a reply queued")
	}
}

// A reply is completed exactly once: by the terminal transition when it is
// still queued, never by it once the writer has taken it.
func TestSendQueue_ReplyCompletedOnce(t *testing.T) {
	q, _ := newTestQueue(-1, -1)
	taken, err := q.pushReply([]byte("taken"))
	if err != nil {
		t.Fatal(err)
	}
	queued, err := q.pushReply([]byte("queued"))
	if err != nil {
		t.Fatal(err)
	}
	it := mustTake(t, q)
	if !q.terminate(causeReleased) {
		t.Fatal("terminate on an open queue reported false")
	}
	if got := <-queued; !errors.Is(got, errConnClosing) {
		t.Fatalf("dropped reply got %v, want errConnClosing", got)
	}
	select {
	case got := <-queued:
		t.Fatalf("dropped reply completed twice (second %v)", got)
	default:
	}
	select {
	case got := <-taken:
		t.Fatalf("the terminal transition completed a reply the writer had taken (%v)", got)
	default:
	}
	it.result <- nil // the writer's completion still fits its slot
	if got := <-taken; got != nil {
		t.Fatalf("taken reply got %v, want the writer's nil", got)
	}
}

// The first terminal cause sticks; later ones and later appends do nothing.
func TestSendQueue_CauseSticks(t *testing.T) {
	q, _ := newTestQueue(-1, -1)
	if !q.terminate(causeQueueFull) {
		t.Fatal("first terminate reported false")
	}
	if q.terminate(causeReleased) || q.terminate(causeServerClose) {
		t.Fatal("a later terminate replaced the cause")
	}
	if got := q.terminalCause(); got != causeQueueFull {
		t.Fatalf("cause %d, want causeQueueFull", got)
	}
	if _, err := q.pushReply([]byte("r")); !errors.Is(err, errConnClosing) {
		t.Fatalf("reply after stop: %v, want errConnClosing", err)
	}
	if q.pushFanout([]byte("f")) {
		t.Fatal("fan-out after stop reported an overflow")
	}
	if n, _ := q.backlog(); n != 0 {
		t.Fatalf("append after stop was queued (%d)", n)
	}
}

// The writer stops the queue when the envelope it takes has waited longer
// than maxAge; an envelope exactly maxAge old is still written.
func TestSendQueue_Stall(t *testing.T) {
	q, clk := newTestQueue(-1, -1)
	q.pushFanout([]byte("old"))
	clk.t = clk.t.Add(time.Second) // exactly maxAge
	mustTake(t, q)

	q.pushFanout([]byte("a"))
	reply, err := q.pushReply([]byte("r"))
	if err != nil {
		t.Fatal(err)
	}
	clk.t = clk.t.Add(time.Second + time.Nanosecond)
	if _, cause := q.take(); cause != causeQueueStalled {
		t.Fatalf("take of an envelope older than maxAge: cause %d, want causeQueueStalled", cause)
	}
	if got := <-reply; !errors.Is(got, errConnClosing) {
		t.Fatalf("reply behind a stalled queue got %v, want errConnClosing", got)
	}
}

// take waits for an append, and for a terminal cause.
func TestSendQueue_TakeWaits(t *testing.T) {
	q, _ := newTestQueue(-1, -1)
	got := make(chan sendItem)
	go func() {
		it, _ := q.take()
		got <- it
	}()
	q.pushFanout([]byte("late"))
	if it := <-got; !bytes.Equal(it.env, []byte("late")) {
		t.Fatalf("took %q, want late", it.env)
	}
	stopped := make(chan closeCause)
	go func() {
		_, cause := q.take()
		stopped <- cause
	}()
	q.terminate(causeReleased)
	if cause := <-stopped; cause != causeReleased {
		t.Fatalf("take after terminate: cause %d, want causeReleased", cause)
	}
}

// The bounds resolve from options like the other caps: zero is the default,
// negative disables, and the byte default follows a larger ReadLimit.
func TestSendQueueBounds_Resolve(t *testing.T) {
	cases := []struct {
		name      string
		opts      Options
		wantLen   int
		wantBytes int64
	}{
		{"defaults", Options{}, 4096, 16 << 20},
		{"explicit", Options{MaxSendQueue: 8, MaxSendQueueBytes: 1000}, 8, 1000},
		{"disabled", Options{MaxSendQueue: -1, MaxSendQueueBytes: -5}, -1, -1},
		{"small ReadLimit keeps 16 MiB", Options{ReadLimit: 1 << 20}, 4096, 16 << 20},
		{"ReadLimit 4 MiB is exactly 16 MiB", Options{ReadLimit: 4 << 20}, 4096, 16 << 20},
		{"large ReadLimit raises it", Options{ReadLimit: 128 << 20}, 4096, 512 << 20},
		{"unlimited ReadLimit keeps 16 MiB", Options{ReadLimit: -1}, 4096, 16 << 20},
		{"huge ReadLimit saturates", Options{ReadLimit: math.MaxInt64 / 2}, 4096, math.MaxInt64},
		{"explicit bytes ignore ReadLimit", Options{ReadLimit: 128 << 20, MaxSendQueueBytes: 1 << 20}, 4096, 1 << 20},
	}
	for _, tc := range cases {
		s := &Server{opts: tc.opts}
		if got := s.maxSendQueue(); got != tc.wantLen {
			t.Errorf("%s: maxSendQueue = %d, want %d", tc.name, got, tc.wantLen)
		}
		if got := s.maxSendQueueBytes(); got != tc.wantBytes {
			t.Errorf("%s: maxSendQueueBytes = %d, want %d", tc.name, got, tc.wantBytes)
		}
	}
}
