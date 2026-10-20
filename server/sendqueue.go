package server

import (
	"errors"
	"sync"
	"time"
)

// errConnClosing is the result of a reply whose connection stopped before its
// writer wrote it, and of a reply queued after that.
var errConnClosing = errors.New("server: connection closing")

// closeCause records why a connection's send queue stopped. It is set once,
// by the first of the events below, and never replaced; the owner of the
// cause closes the socket (see conn.writeLoop and conn.closeFromServer).
type closeCause uint8

const (
	causeNone         closeCause = iota
	causeQueueFull               // a fan-out append found the queue at a bound; the writer closes with 1013
	causeQueueStalled            // the writer took an envelope older than WriteTimeout; the writer closes with 1013
	causeWriteFailed             // a write failed or hit its deadline; the writer closes with 1008
	causeServerClose             // the server closes the socket itself with its own code
	causeReleased                // the connection is being released; nobody sends a close frame
)

// sendItem is one queued envelope. A reply carries a one-slot result
// channel, filled exactly once: by the writer after its write, or by the
// terminal transition when it drops the reply. Fan-out has none.
type sendItem struct {
	env    []byte
	queued time.Time
	result chan error
}

// sendQueue is a connection's outgoing FIFO, drained by one writer goroutine
// (conn.writeLoop), so no producer ever waits for a socket.
//
// Two kinds of envelope go through it. A reply is what the connection's own
// goroutine sends through the sync handler's Send (the initial sync, and the
// replies HandleFrame makes); the caller waits until the writer has written
// it, so a connection has at most one reply queued and a client that floods
// requests without reading slows only its own read loop. Fan-out is what
// any goroutine sends to the connection on behalf of the room (broadcasts,
// presence, sweep and disconnect tombstones, backplane re-broadcasts); it
// returns at once and is bounded: an append that finds maxLen envelopes or
// maxBytes bytes of fan-out already queued stops the queue instead
// (causeQueueFull). Replies are not counted against the bounds, so a
// document-sized SyncStep2 never trips them. The writer also stops the queue
// when the envelope it takes has waited longer than maxAge
// (causeQueueStalled), which bounds how long a reply can wait behind a
// backlog that drains at exactly the rate it grows.
//
// Stopping drops every queued envelope and fails the queued replies; the
// lock is never held across a socket call or a wait.
type sendQueue struct {
	maxLen   int           // fan-out envelopes; negative = no bound
	maxBytes int64         // fan-out bytes; negative = no bound
	maxAge   time.Duration // how long an envelope may wait to be taken
	now      func() time.Time

	mu     sync.Mutex
	items  []sendItem // items[0] is the next envelope the writer takes
	fanout int        // fan-out envelopes in items
	bytes  int64      // their bytes
	total  int64      // bytes of every item in items, replies included
	cause  closeCause
	wake   chan struct{} // one slot: an append or a terminal cause
	done   chan struct{} // closed when the writer exits; nil if it never started
}

func newSendQueue(maxLen int, maxBytes int64, maxAge time.Duration) *sendQueue {
	return &sendQueue{
		maxLen:   maxLen,
		maxBytes: maxBytes,
		maxAge:   maxAge,
		now:      time.Now,
		wake:     make(chan struct{}, 1),
	}
}

// signal wakes the writer without blocking. Called with mu held.
func (q *sendQueue) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// pushReply queues a reply and returns the channel its result arrives on, or
// errConnClosing when the queue has stopped.
func (q *sendQueue) pushReply(env []byte) (<-chan error, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.cause != causeNone {
		return nil, errConnClosing
	}
	ch := make(chan error, 1)
	q.items = append(q.items, sendItem{env: env, queued: q.now(), result: ch})
	q.total += int64(len(env))
	q.signal()
	return ch, nil
}

// pushFanout queues fan-out. When the queue already holds maxLen fan-out
// envelopes, or maxBytes or more fan-out bytes, it stops the queue with
// causeQueueFull instead and reports true, so the caller counts the overflow
// once. One envelope is never refused for its own size. An append after the
// queue stopped is dropped.
func (q *sendQueue) pushFanout(env []byte) (overflowed bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.cause != causeNone {
		return false
	}
	if (q.maxLen >= 0 && q.fanout >= q.maxLen) || (q.maxBytes >= 0 && q.bytes >= q.maxBytes) {
		q.terminateLocked(causeQueueFull)
		return true
	}
	q.items = append(q.items, sendItem{env: env, queued: q.now()})
	q.fanout++
	q.bytes += int64(len(env))
	q.total += int64(len(env))
	q.signal()
	return false
}

// take removes the next envelope for the writer, waiting until there is one.
// It returns the terminal cause instead once one is set, and sets
// causeQueueStalled itself when the next envelope has waited longer than
// maxAge.
func (q *sendQueue) take() (sendItem, closeCause) {
	for {
		q.mu.Lock()
		if q.cause != causeNone {
			cause := q.cause
			q.mu.Unlock()
			return sendItem{}, cause
		}
		if len(q.items) > 0 {
			if q.now().Sub(q.items[0].queued) > q.maxAge {
				q.terminateLocked(causeQueueStalled)
				q.mu.Unlock()
				return sendItem{}, causeQueueStalled
			}
			it := q.items[0]
			q.items[0] = sendItem{} // let the written envelope go
			q.items = q.items[1:]
			if len(q.items) == 0 {
				q.items = nil
			}
			if it.result == nil {
				q.fanout--
				q.bytes -= int64(len(it.env))
			}
			q.total -= int64(len(it.env))
			q.mu.Unlock()
			return it, causeNone
		}
		q.mu.Unlock()
		<-q.wake
	}
}

// terminate sets the terminal cause unless one is already set, and reports
// whether this call set it.
func (q *sendQueue) terminate(cause closeCause) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.terminateLocked(cause)
}

func (q *sendQueue) terminateLocked(cause closeCause) bool {
	if q.cause != causeNone {
		return false
	}
	q.cause = cause
	for i := range q.items {
		if r := q.items[i].result; r != nil {
			r <- errConnClosing // one slot, never full: a reply is completed once
		}
		q.items[i] = sendItem{}
	}
	q.items = nil
	q.fanout, q.bytes, q.total = 0, 0, 0
	q.signal()
	return true
}

// terminalCause returns the cause set so far, or causeNone.
func (q *sendQueue) terminalCause() closeCause {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.cause
}

// backlog returns the envelopes queued and not yet taken, and their bytes,
// replies included. A nil queue (a connection built without one) is empty.
func (q *sendQueue) backlog() (int, int64) {
	if q == nil {
		return 0, 0
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items), q.total
}
