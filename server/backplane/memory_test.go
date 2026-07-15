package backplane

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// collector records the updates delivered to one subscription and lets a
// test await a given count without polling.
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
	deadline := time.After(2 * time.Second)
	for i := 0; i < n; i++ {
		select {
		case <-c.ping:
		case <-deadline:
			t.Fatalf("timed out waiting for delivery %d/%d", i+1, n)
		}
	}
}

func (c *collector) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.got)
}

func (c *collector) at(i int) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.got[i]
}

// TestMemory_PublishSkipsSelfDeliversToOthers: a publish reaches other
// connections' subscriptions but never the publisher's own (no echo).
func TestMemory_PublishSkipsSelfDeliversToOthers(t *testing.T) {
	hub := NewMemory()
	defer hub.Close()
	a := hub.Conn()
	b := hub.Conn()

	ca, cb := newCollector(), newCollector()
	unsubA, err := a.Subscribe("doc", ca.onUpdate)
	if err != nil {
		t.Fatal(err)
	}
	defer unsubA()
	unsubB, err := b.Subscribe("doc", cb.onUpdate)
	if err != nil {
		t.Fatal(err)
	}
	defer unsubB()

	if err := a.Publish(context.Background(), "doc", []byte{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	cb.await(t, 1)
	if got := cb.at(0); !bytes.Equal(got, []byte{1, 2, 3}) {
		t.Fatalf("b got %v, want [1 2 3]", got)
	}
	// Give any erroneous self-delivery time to arrive, then confirm none did.
	time.Sleep(50 * time.Millisecond)
	if n := ca.count(); n != 0 {
		t.Fatalf("publisher received its own publish (%d deliveries)", n)
	}
}

// TestMemory_DeliversInPublishOrder: a single subscription sees updates in
// the order they were published (one draining goroutine per subscription).
func TestMemory_DeliversInPublishOrder(t *testing.T) {
	hub := NewMemory()
	defer hub.Close()
	a, b := hub.Conn(), hub.Conn()
	cb := newCollector()
	unsub, err := b.Subscribe("d", cb.onUpdate)
	if err != nil {
		t.Fatal(err)
	}
	defer unsub()

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

// TestMemory_UnsubStopsDelivery: after unsub the subscription is removed and
// receives nothing further; unsub is idempotent.
func TestMemory_UnsubStopsDelivery(t *testing.T) {
	hub := NewMemory()
	defer hub.Close()
	a, b := hub.Conn(), hub.Conn()
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
	hub.mu.Lock()
	remaining := len(hub.subs["d"])
	hub.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("subscriptions remain after unsub: %d", remaining)
	}

	if err := a.Publish(context.Background(), "d", []byte{2}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if n := cb.count(); n != 1 {
		t.Fatalf("delivery after unsub: got %d, want 1", n)
	}
	unsub() // idempotent, must not panic
}

// TestMemory_IsolatesDocs: a subscription only sees publishes for its own
// docName.
func TestMemory_IsolatesDocs(t *testing.T) {
	hub := NewMemory()
	defer hub.Close()
	a, b := hub.Conn(), hub.Conn()
	cb := newCollector()
	unsub, err := b.Subscribe("doc1", cb.onUpdate)
	if err != nil {
		t.Fatal(err)
	}
	defer unsub()

	if err := a.Publish(context.Background(), "doc2", []byte{9}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if n := cb.count(); n != 0 {
		t.Fatalf("doc1 subscriber received a doc2 publish (%d)", n)
	}
}

// TestMemory_ClosedReturnsErr: after the hub is closed, Publish and
// Subscribe on any connection return ErrClosed; Close is idempotent.
func TestMemory_ClosedReturnsErr(t *testing.T) {
	hub := NewMemory()
	a := hub.Conn()
	if err := hub.Close(); err != nil {
		t.Fatal(err)
	}
	if err := hub.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := a.Subscribe("d", func([]byte) {}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Subscribe after Close = %v, want ErrClosed", err)
	}
	if err := a.Publish(context.Background(), "d", []byte{1}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Publish after Close = %v, want ErrClosed", err)
	}
}

// TestMemory_ConnCloseStopsOwnSubsOnly: closing one connection stops its
// subscriptions but leaves other connections delivering.
func TestMemory_ConnCloseStopsOwnSubsOnly(t *testing.T) {
	hub := NewMemory()
	defer hub.Close()
	a, b, pub := hub.Conn(), hub.Conn(), hub.Conn()
	ca, cb := newCollector(), newCollector()
	if _, err := a.Subscribe("d", ca.onUpdate); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Subscribe("d", cb.onUpdate); err != nil {
		t.Fatal(err)
	}

	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if err := pub.Publish(context.Background(), "d", []byte{7}); err != nil {
		t.Fatal(err)
	}
	cb.await(t, 1) // b still delivers
	time.Sleep(50 * time.Millisecond)
	if n := ca.count(); n != 0 {
		t.Fatalf("connection kept delivering after Conn.Close (%d)", n)
	}
}
