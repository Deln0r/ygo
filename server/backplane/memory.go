package backplane

import (
	"context"
	"errors"
	"sync"
)

// ErrClosed is returned by a Backplane whose hub has been Closed.
var ErrClosed = errors.New("backplane: closed")

// Memory is an in-process Backplane hub. Every server that should share
// documents takes its own connection from the same hub via Conn; a
// connection never receives its own publishes. It targets tests and
// single-process multi-instance setups. Delivery is asynchronous and
// ordered per subscription (each subscription is drained by one goroutine),
// faithfully modelling a broker without the network. For sharing across
// machines, use a broker-backed Backplane instead.
//
// The zero value is not usable; construct with NewMemory.
type Memory struct {
	mu      sync.Mutex
	subs    map[string]map[uint64]*memSub // docName -> subID -> sub
	nextIID uint64                        // instance-identity counter (Conn)
	nextSub uint64                        // subscription-id counter
	closed  bool
}

// NewMemory returns an empty in-process hub.
func NewMemory() *Memory {
	return &Memory{subs: map[string]map[uint64]*memSub{}}
}

// Conn returns a Backplane bound to this hub with a fresh instance
// identity. Give each server its own Conn; publishes from one Conn are
// never delivered to that same Conn's subscriptions.
func (m *Memory) Conn() Backplane {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextIID++
	return &memConn{hub: m, origin: m.nextIID, done: make(chan struct{})}
}

// Close tears the whole hub down: every subscription across every Conn is
// stopped and further Publish/Subscribe on any Conn returns ErrClosed.
// Intended for test cleanup; a single server leaving should Close only its
// own Conn (which stops just that Conn's subscriptions). Safe to call more
// than once.
func (m *Memory) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	var stop []*memSub
	for _, byID := range m.subs {
		for _, s := range byID {
			stop = append(stop, s)
		}
	}
	m.subs = map[string]map[uint64]*memSub{}
	m.mu.Unlock()
	for _, s := range stop {
		s.stop()
	}
	return nil
}

// memSub is one live subscription: a doc channel drained by a single
// goroutine that invokes onUpdate in publish order.
type memSub struct {
	origin    uint64
	onUpdate  func([]byte)
	ch        chan []byte
	done      chan struct{}
	closeOnce sync.Once
}

// memSubBuffer bounds how many undelivered updates a slow subscriber may
// queue before publishers block on it (backpressure). Generous for any
// real edit cadence.
const memSubBuffer = 256

func (s *memSub) stop() { s.closeOnce.Do(func() { close(s.done) }) }

func (s *memSub) run() {
	for {
		// Prefer teardown: once stopped, do not start another onUpdate.
		select {
		case <-s.done:
			return
		default:
		}
		select {
		case <-s.done:
			return
		case u := <-s.ch:
			s.onUpdate(u)
		}
	}
}

// memConn is one server's handle on the hub, carrying its instance origin.
type memConn struct {
	hub       *Memory
	origin    uint64
	done      chan struct{} // closed by Close, to unblock this conn's in-flight Publish
	closeOnce sync.Once
}

func (c *memConn) Publish(ctx context.Context, docName string, update []byte) error {
	select {
	case <-c.done:
		return ErrClosed
	default:
	}
	c.hub.mu.Lock()
	if c.hub.closed {
		c.hub.mu.Unlock()
		return ErrClosed
	}
	var targets []*memSub
	for _, s := range c.hub.subs[docName] {
		if s.origin != c.origin { // never echo to the publisher
			targets = append(targets, s)
		}
	}
	c.hub.mu.Unlock()

	if len(targets) == 0 {
		return nil
	}
	// One read-only copy shared by all targets; the caller may reuse update.
	cp := append([]byte(nil), update...)
	for _, s := range targets {
		select {
		case s.ch <- cp:
		case <-s.done: // subscription torn down mid-publish; skip it
		case <-c.done: // this connection closed mid-publish; abandon (unblocks
			// a publisher wedged on a slow peer when its own server Closes)
			return ErrClosed
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (c *memConn) Subscribe(docName string, onUpdate func([]byte)) (func(), error) {
	c.hub.mu.Lock()
	if c.hub.closed {
		c.hub.mu.Unlock()
		return nil, ErrClosed
	}
	c.hub.nextSub++
	id := c.hub.nextSub
	sub := &memSub{
		origin:   c.origin,
		onUpdate: onUpdate,
		ch:       make(chan []byte, memSubBuffer),
		done:     make(chan struct{}),
	}
	if c.hub.subs[docName] == nil {
		c.hub.subs[docName] = map[uint64]*memSub{}
	}
	c.hub.subs[docName][id] = sub
	c.hub.mu.Unlock()

	go sub.run()

	var once sync.Once
	unsub := func() {
		once.Do(func() {
			c.hub.mu.Lock()
			if byID := c.hub.subs[docName]; byID != nil {
				delete(byID, id)
				if len(byID) == 0 {
					delete(c.hub.subs, docName)
				}
			}
			c.hub.mu.Unlock()
			sub.stop()
		})
	}
	return unsub, nil
}

// Close stops every subscription this Conn still holds and unblocks any of
// its in-flight Publish calls (a publisher wedged on a slow peer's full
// buffer returns ErrClosed rather than outliving the closing server).
// Idempotent. It does not tear down the hub or other Conns.
func (c *memConn) Close() error {
	c.closeOnce.Do(func() { close(c.done) })
	c.hub.mu.Lock()
	var stop []*memSub
	for docName, byID := range c.hub.subs {
		for id, s := range byID {
			if s.origin == c.origin {
				stop = append(stop, s)
				delete(byID, id)
			}
		}
		if len(byID) == 0 {
			delete(c.hub.subs, docName)
		}
	}
	c.hub.mu.Unlock()
	for _, s := range stop {
		s.stop()
	}
	return nil
}
