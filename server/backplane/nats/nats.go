// Package nats provides a NATS-backed Backplane for the ygo server, fanning
// document updates between server instances across machines.
//
// It satisfies github.com/Deln0r/ygo/server/backplane.Backplane: point every
// instance's server.Options.Backplane at one built from a shared NATS
// connection and the instances converge on shared documents. As with any
// backplane, a shared Store is still required (foreign updates are applied in
// memory only, not re-persisted). Payloads are opaque to the adapter, so both
// document updates and presence/awareness are carried.
//
// Delivery follows core NATS semantics: at-most-once, fire-and-forget. A
// dropped message is a dropped causal dependency, not just one lost edit: it
// silently parks every later edit from that client on the receiving instance
// (the apply reports no error), and the shared Store heals it only when that
// document is next (re)loaded — on eviction and reload. A continuously
// resident document (always at least one client) never reloads, so for a hot
// document a single dropped delta is not automatically healed. Where that is
// unacceptable, use the JetStream-backed backplane in this package
// (NewJetStream): it publishes into a persistent stream and consumes with an
// ordered consumer that resumes from the last delivered message across a
// reconnect or server restart, so a transient outage does not silently lose
// delivery (bounded by the stream's retention). Core NATS matches the y-redis
// model.
//
// This is a separate Go module so the ygo core stays dependency-free;
// adopters that want NATS clustering opt in by importing it.
package nats

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"sync"

	"github.com/nats-io/nats.go"

	"github.com/Deln0r/ygo/server/backplane"
)

// Compile-time guarantee that the adapter satisfies the ygo Backplane
// contract, so an interface change fails this module's build rather than an
// adopter's.
var _ backplane.Backplane = (*Backplane)(nil)

// DefaultPrefix is the NATS subject prefix used when WithPrefix is not given.
const DefaultPrefix = "ygo"

// originHeader carries the publishing instance's identity so a subscriber
// skips its own publishes: NATS delivers a publish to every subscription on
// the subject, including the publisher's own.
const originHeader = "Ygo-Origin"

// ErrClosed is returned by a Backplane after Close.
var ErrClosed = errors.New("nats backplane: closed")

// Backplane fans ygo document updates over NATS. Construct with New; it is
// safe for concurrent use and satisfies backplane.Backplane.
type Backplane struct {
	nc     *nats.Conn
	prefix string
	origin string // unique per instance; filters this instance's own publishes

	mu     sync.Mutex
	subs   map[*nats.Subscription]struct{}
	closed bool
}

// Option configures a Backplane.
type Option func(*Backplane)

// WithPrefix sets the NATS subject prefix (default "ygo"). Every instance
// sharing documents must use the same prefix.
func WithPrefix(p string) Option { return func(b *Backplane) { b.prefix = p } }

// New returns a Backplane that publishes and subscribes over nc. The caller
// owns nc — its authentication, TLS, and reconnect policy, and its lifetime:
// Close releases only the Backplane's own subscriptions and does NOT close
// nc. Each Backplane takes a fresh random instance identity, so publishes
// from one are never delivered to that same instance's handlers.
func New(nc *nats.Conn, opts ...Option) (*Backplane, error) {
	if nc == nil {
		return nil, errors.New("nats backplane: nil connection")
	}
	b := &Backplane{
		nc:     nc,
		prefix: DefaultPrefix,
		origin: newOrigin(),
		subs:   map[*nats.Subscription]struct{}{},
	}
	for _, o := range opts {
		o(b)
	}
	if b.prefix == "" {
		// An empty prefix would make every subject start with an invalid
		// empty token; fail fast rather than silently drop every publish.
		return nil, errors.New("nats backplane: empty prefix")
	}
	return b, nil
}

func newOrigin() string {
	var raw [16]byte
	_, _ = rand.Read(raw[:])
	return hex.EncodeToString(raw[:])
}

// docSubject maps a docName to a NATS subject token under prefix. docName is
// base64url-encoded so arbitrary bytes — dots (which would split into
// sub-subjects), spaces, and the '*'/'>' wildcards — cannot break subject
// routing or leak a document's updates onto another's channel. Shared by the
// core-NATS and JetStream backplanes so both encode identically.
func docSubject(prefix, docName string) string {
	tok := base64.RawURLEncoding.EncodeToString([]byte(docName))
	if tok == "" {
		// Only the empty docName encodes to an empty token, which would form
		// an invalid "prefix." subject (trailing empty token). Map it to a
		// single-char token: base64 of >=1 byte is always >=2 chars, so a
		// one-char token cannot collide with any non-empty docName.
		tok = "_"
	}
	return prefix + "." + tok
}

func (b *Backplane) subject(docName string) string { return docSubject(b.prefix, docName) }

// Publish sends update on docName's subject to every OTHER subscribed
// instance, tagging it with this instance's origin so subscribers skip their
// own publishes. It returns promptly on a cancelled ctx or after Close.
func (b *Backplane) Publish(ctx context.Context, docName string, update []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.Lock()
	closed := b.closed
	b.mu.Unlock()
	if closed {
		return ErrClosed
	}
	msg := nats.NewMsg(b.subject(docName))
	msg.Data = update
	msg.Header.Set(originHeader, b.origin)
	return b.nc.PublishMsg(msg)
}

// Subscribe delivers foreign updates for docName (those published by other
// instances) to onUpdate, returning an unsubscribe func. onUpdate runs on the
// subscription's own delivery goroutine, one call at a time in receive order;
// unsub is safe to call more than once.
func (b *Backplane) Subscribe(docName string, onUpdate func(update []byte)) (func(), error) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil, ErrClosed
	}
	b.mu.Unlock()

	sub, err := b.nc.Subscribe(b.subject(docName), func(m *nats.Msg) {
		if m.Header.Get(originHeader) == b.origin {
			return // our own publish echoed back; skip
		}
		onUpdate(m.Data)
	})
	if err != nil {
		return nil, err
	}
	// Flush so the subscription is registered on the server before Subscribe
	// returns. nats.Subscribe only queues the interest; a publish that raced
	// an un-flushed Subscribe would be dropped, defeating the server's
	// subscribe-before-load ordering. (This guarantees registration on the
	// client's own NATS server; in a multi-server NATS cluster, interest
	// still propagates asynchronously across servers — the shared Store is
	// the backstop for that narrow window, as for any dropped message.)
	if err := b.nc.Flush(); err != nil {
		_ = sub.Unsubscribe()
		return nil, err
	}

	b.mu.Lock()
	if b.closed {
		// Closed between the guard above and registration; do not leak the sub.
		b.mu.Unlock()
		_ = sub.Unsubscribe()
		return nil, ErrClosed
	}
	b.subs[sub] = struct{}{}
	b.mu.Unlock()

	var once sync.Once
	unsub := func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subs, sub)
			b.mu.Unlock()
			_ = sub.Unsubscribe()
		})
	}
	return unsub, nil
}

// Close unsubscribes every subscription this Backplane still holds and blocks
// further Publish/Subscribe (ErrClosed). It does NOT close the underlying
// NATS connection, which the caller owns. Idempotent.
func (b *Backplane) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	subs := make([]*nats.Subscription, 0, len(b.subs))
	for s := range b.subs {
		subs = append(subs, s)
	}
	b.subs = map[*nats.Subscription]struct{}{}
	b.mu.Unlock()

	for _, s := range subs {
		_ = s.Unsubscribe()
	}
	return nil
}
