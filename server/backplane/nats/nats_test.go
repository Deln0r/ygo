package nats

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/test"
	"github.com/nats-io/nats.go"
)

// runServer starts an embedded NATS server on a random port and returns its
// client URL; it is shut down at test end.
func runServer(t *testing.T) string {
	t.Helper()
	opts := natsserver.DefaultTestOptions
	opts.Port = -1
	s := natsserver.RunServer(&opts)
	t.Cleanup(s.Shutdown)
	return s.ClientURL()
}

func dial(t *testing.T, url string) *nats.Conn {
	t.Helper()
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)
	return nc
}

func newBackplane(t *testing.T, url string, opts ...Option) *Backplane {
	t.Helper()
	b, err := New(dial(t, url), opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

// collector records delivered updates and lets a test await a count without
// polling.
type collector struct {
	mu   sync.Mutex
	got  [][]byte
	ping chan struct{}
}

func newCollector() *collector { return &collector{ping: make(chan struct{}, 256)} }
func (c *collector) onUpdate(u []byte) {
	c.mu.Lock()
	c.got = append(c.got, append([]byte(nil), u...))
	c.mu.Unlock()
	c.ping <- struct{}{}
}
func (c *collector) await(t *testing.T, n int) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for i := 0; i < n; i++ {
		select {
		case <-c.ping:
		case <-deadline:
			t.Fatalf("timed out waiting for delivery %d/%d", i+1, n)
		}
	}
}
func (c *collector) count() int { c.mu.Lock(); defer c.mu.Unlock(); return len(c.got) }
func (c *collector) at(i int) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.got[i]
}

// TestBackplane_PublishSkipsSelfDeliversToOthers: a publish reaches other
// instances' subscriptions but never the publisher's own (origin-filtered).
func TestBackplane_PublishSkipsSelfDeliversToOthers(t *testing.T) {
	url := runServer(t)
	a := newBackplane(t, url)
	b := newBackplane(t, url)

	ca, cb := newCollector(), newCollector()
	unsubA, err := a.Subscribe("doc", ca.onUpdate)
	if err != nil {
		t.Fatal(err)
	}
	defer unsubA()
	if _, err := b.Subscribe("doc", cb.onUpdate); err != nil {
		t.Fatal(err)
	}

	if err := a.Publish(context.Background(), "doc", []byte{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	cb.await(t, 1)
	if got := cb.at(0); !bytes.Equal(got, []byte{1, 2, 3}) {
		t.Fatalf("b got %v, want [1 2 3]", got)
	}
	time.Sleep(100 * time.Millisecond) // let any wrong self-delivery arrive
	if n := ca.count(); n != 0 {
		t.Fatalf("publisher received its own publish (%d deliveries)", n)
	}
}

// TestBackplane_DeliversInReceiveOrder: a subscription's single delivery
// goroutine invokes onUpdate in order.
func TestBackplane_DeliversInReceiveOrder(t *testing.T) {
	url := runServer(t)
	a := newBackplane(t, url)
	b := newBackplane(t, url)
	cb := newCollector()
	if _, err := b.Subscribe("d", cb.onUpdate); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		if err := a.Publish(context.Background(), "d", []byte{byte(i)}); err != nil {
			t.Fatal(err)
		}
	}
	cb.await(t, 20)
	for i := 0; i < 20; i++ {
		if got := cb.at(i)[0]; got != byte(i) {
			t.Fatalf("delivery %d = %d, want %d (out of order)", i, got, i)
		}
	}
}

// TestBackplane_IsolatesDocs: a subscription only sees its own docName's
// publishes.
func TestBackplane_IsolatesDocs(t *testing.T) {
	url := runServer(t)
	a := newBackplane(t, url)
	b := newBackplane(t, url)
	cb := newCollector()
	if _, err := b.Subscribe("doc1", cb.onUpdate); err != nil {
		t.Fatal(err)
	}
	if err := a.Publish(context.Background(), "doc2", []byte{9}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if n := cb.count(); n != 0 {
		t.Fatalf("doc1 subscriber received a doc2 publish (%d)", n)
	}
}

// TestBackplane_SubjectSafeForSpecialDocNames guards the base64url subject
// encoding: docNames containing NATS subject metacharacters ('.', '*', '>')
// must route to distinct, non-overlapping channels, so a publish to one is
// never delivered to a subscriber of another.
func TestBackplane_SubjectSafeForSpecialDocNames(t *testing.T) {
	url := runServer(t)
	a := newBackplane(t, url)
	b := newBackplane(t, url)

	// A subscriber on the wildcard-looking docName ">" must NOT receive a
	// publish meant for the unrelated docName "a.b" (raw, ">" would match all).
	cWild := newCollector()
	if _, err := b.Subscribe(">", cWild.onUpdate); err != nil {
		t.Fatal(err)
	}
	// A subscriber on the exact docName "a.b" that should receive it.
	cExact := newCollector()
	if _, err := b.Subscribe("a.b", cExact.onUpdate); err != nil {
		t.Fatal(err)
	}

	if err := a.Publish(context.Background(), "a.b", []byte{7}); err != nil {
		t.Fatal(err)
	}
	cExact.await(t, 1) // the intended subscriber gets it
	time.Sleep(100 * time.Millisecond)
	if n := cWild.count(); n != 0 {
		t.Fatalf("wildcard-named docName leaked %d cross-doc messages", n)
	}
}

// TestBackplane_UnsubStopsDelivery: after unsub the subscription receives
// nothing; unsub is idempotent.
func TestBackplane_UnsubStopsDelivery(t *testing.T) {
	url := runServer(t)
	a := newBackplane(t, url)
	b := newBackplane(t, url)
	cb := newCollector()
	unsub, err := b.Subscribe("d", cb.onUpdate)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Publish(context.Background(), "d", []byte{1}); err != nil {
		t.Fatal(err)
	}
	cb.await(t, 1)

	unsub()
	if err := a.Publish(context.Background(), "d", []byte{2}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	if n := cb.count(); n != 1 {
		t.Fatalf("delivery after unsub: got %d, want 1", n)
	}
	unsub() // idempotent
}

// TestBackplane_ClosedReturnsErr: after Close, Publish and Subscribe return
// ErrClosed; Close is idempotent.
func TestBackplane_ClosedReturnsErr(t *testing.T) {
	url := runServer(t)
	b, err := New(dial(t, url))
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := b.Subscribe("d", func([]byte) {}); err != ErrClosed {
		t.Fatalf("Subscribe after Close = %v, want ErrClosed", err)
	}
	if err := b.Publish(context.Background(), "d", []byte{1}); err != ErrClosed {
		t.Fatalf("Publish after Close = %v, want ErrClosed", err)
	}
}

// TestBackplane_PublishHonorsContext: a cancelled context returns before any
// network work.
func TestBackplane_PublishHonorsContext(t *testing.T) {
	url := runServer(t)
	b := newBackplane(t, url)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := b.Publish(ctx, "d", []byte{1}); err == nil {
		t.Fatal("Publish with cancelled ctx returned nil, want ctx error")
	}
}

// TestBackplane_EmptyDocNameRoutesAndIsolates: the empty docName is a valid,
// isolated channel (matching the in-memory hub), not an invalid subject. Its
// updates reach an empty-docName subscriber and no other document's.
func TestBackplane_EmptyDocNameRoutesAndIsolates(t *testing.T) {
	url := runServer(t)
	a := newBackplane(t, url)
	b := newBackplane(t, url)

	cEmpty, cReal := newCollector(), newCollector()
	if _, err := b.Subscribe("", cEmpty.onUpdate); err != nil {
		t.Fatalf(`Subscribe("") = %v, want nil`, err)
	}
	if _, err := b.Subscribe("real", cReal.onUpdate); err != nil {
		t.Fatal(err)
	}
	if err := a.Publish(context.Background(), "", []byte{1}); err != nil {
		t.Fatalf(`Publish("") = %v, want nil`, err)
	}
	cEmpty.await(t, 1)
	time.Sleep(100 * time.Millisecond)
	if n := cReal.count(); n != 0 {
		t.Fatalf(`empty-docName publish leaked to "real" (%d)`, n)
	}
}

// TestBackplane_EmptyPrefixRejected: WithPrefix("") fails fast in New rather
// than making every subject invalid.
func TestBackplane_EmptyPrefixRejected(t *testing.T) {
	url := runServer(t)
	if _, err := New(dial(t, url), WithPrefix("")); err == nil {
		t.Fatal("New with empty prefix returned nil error, want rejection")
	}
}
