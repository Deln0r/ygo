// Package backplane fans document updates between ygo server instances
// that share a document, so several horizontally-scaled servers behind a
// load balancer converge on the same document.
//
// Each server Publishes every applied document update on that document's
// channel and Subscribes to foreign updates for every document it holds
// resident; on a foreign update it applies the bytes to its in-memory copy
// and re-broadcasts to its own connected clients. Yjs V1 updates are
// idempotent (applying one twice is a no-op) and commutative, so
// convergence is order-independent: a backplane only has to deliver every
// update at least once and never echo an update back to the instance that
// published it.
//
// The package ships the Backplane interface plus Memory, an in-process
// implementation for tests and single-process multi-instance setups.
// A cross-machine broker adapter (e.g. Redis or NATS) lives in its own
// package so the core module stays dependency-free; it only has to satisfy
// the Backplane contract here.
package backplane

import "context"

// Backplane is the cross-instance pub/sub transport. A single server holds
// one Backplane (typically one connection to a shared broker) for its
// lifetime. Implementations must be safe for concurrent use.
type Backplane interface {
	// Publish sends update on docName's channel to every OTHER subscribed
	// instance. It must not deliver the update back to the publishing
	// instance's own Subscribe handler — the instance already applied it
	// locally, and a re-delivery would cause an echo loop; implementations
	// self-filter by an instance identity fixed at construction. Publish
	// must honor ctx cancellation and must return promptly (not block past
	// Close) if the Backplane is Closed concurrently, so a shutting-down
	// server never wedges on a stalled peer.
	Publish(ctx context.Context, docName string, update []byte) error

	// Subscribe registers onUpdate to receive foreign updates for docName
	// (those published by other instances). It returns an unsubscribe func
	// that stops delivery; calling it more than once is safe. onUpdate runs
	// on a backplane-owned goroutine, one call at a time per subscription
	// and in publish order, so the handler need not be reentrant; it should
	// not block indefinitely, as a stalled handler applies backpressure to
	// the publisher.
	Subscribe(docName string, onUpdate func(update []byte)) (unsub func(), err error)

	// Close releases the transport. After Close, Publish and Subscribe
	// return an error. Safe to call more than once.
	Close() error
}
