// Package client is a Yjs sync provider over WebSocket: the Go
// equivalent of y-websocket's WebsocketProvider, compatible with
// yserve, Hocuspocus, and the reference y-websocket server.
//
// A Client owns a live connection for one document: it runs the
// y-protocols handshake (SyncStep1 / SyncStep2), broadcasts local
// transactions as incremental SyncUpdate frames, applies remote
// frames to the local Doc, exchanges awareness state, and reconnects
// with exponential backoff until closed.
//
// Local edits made through the Doc between (and during) connections
// are never lost: each (re)connect handshake diffs against the
// server's state vector, so offline edits flow up and missed remote
// edits flow down. This is the embeddable building block for bots,
// CLI tools, server-side agents, and the gomobile bindings.
//
// Concurrency: while a Client is connected, remote updates commit on
// background goroutines. Read document state under a read transaction
// (Doc.ReadTxn) and resolve root types via Doc.Branch OUTSIDE any
// open transaction; the shared-type wrappers' lock-free read methods
// are only safe when the caller holds a transaction.
package client

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/Deln0r/ygo/internal/awareness"
	"github.com/Deln0r/ygo/internal/doc"
	"github.com/Deln0r/ygo/internal/encoding"
	"github.com/Deln0r/ygo/internal/store"
	syncpkg "github.com/Deln0r/ygo/internal/sync"
	"github.com/Deln0r/ygo/persist"
)

// Options configures a Client. URL and DocName are required; the
// zero value of every other field selects a sensible default.
type Options struct {
	// URL is the server base, e.g. "ws://localhost:8080" or
	// "wss://collab.example.com". The document name is appended as
	// the URL path (y-websocket convention).
	URL string

	// DocName addresses the document on the server.
	DocName string

	// Doc is the local document the client syncs. When nil a fresh
	// Doc is created; retrieve it via Client.Doc.
	Doc *doc.Doc

	// Awareness optionally carries per-client ephemeral state
	// (cursor, name). When nil a fresh Awareness bound to the Doc's
	// clientID is created.
	Awareness *awareness.Awareness

	// OnSynced fires on synced-state transitions: true after each
	// completed handshake, false on every disconnect. Called from the
	// client's goroutines; keep it fast and do not call back into
	// blocking Client methods.
	OnSynced func(synced bool)

	// OnError observes non-fatal connection errors (dial failures,
	// dropped connections). The client keeps reconnecting regardless.
	OnError func(err error)

	// MinBackoff / MaxBackoff bound the reconnect backoff. Defaults:
	// 250ms / 10s.
	MinBackoff time.Duration
	MaxBackoff time.Duration

	// LocalStore, when set, makes the client offline-first: on Connect
	// the document loads its persisted state from the store (so it is
	// usable before any network), every subsequent change (local edit
	// or applied remote update) is persisted, and on reconnect the
	// handshake carries any edits made while offline up to the server.
	// Keyed by DocName. A pure-Go on-device sqlite store
	// (persist/sqlite) is the typical mobile choice. nil disables
	// local persistence; the document lives only in memory.
	LocalStore persist.Store
}

// Client is a live sync session for one document. Construct with New,
// start with Connect, stop with Close. Safe for concurrent use.
type Client struct {
	opts      Options
	doc       *doc.Doc
	awareness *awareness.Awareness

	cancel      context.CancelFunc
	done        chan struct{}
	unsubscribe func()

	mu       sync.Mutex
	conn     *websocket.Conn
	connCtx  context.Context
	synced   bool
	lastSent store.StateVector

	dirtyDoc       chan struct{}
	dirtyAwareness chan struct{}

	// closed guards Close so its teardown runs exactly once even under
	// concurrent / repeated calls.
	closed bool

	// Offline persistence state (active only when opts.LocalStore set).
	// lastPersisted is the state-vector watermark of what has been
	// written to the store; the persist loop stores the diff past it on
	// each change. persistDone closes when the persist goroutine exits.
	dirtyPersist  chan struct{}
	persistDone   chan struct{}
	lastPersisted store.StateVector
	persistCount  int
}

// New validates opts and returns an unconnected Client.
func New(opts Options) (*Client, error) {
	if opts.URL == "" {
		return nil, errors.New("client: Options.URL is required")
	}
	if opts.DocName == "" {
		return nil, errors.New("client: Options.DocName is required")
	}
	if opts.MinBackoff <= 0 {
		opts.MinBackoff = 250 * time.Millisecond
	}
	if opts.MaxBackoff <= 0 {
		opts.MaxBackoff = 10 * time.Second
	}
	d := opts.Doc
	if d == nil {
		d = doc.NewDoc()
	}
	a := opts.Awareness
	if a == nil {
		a = awareness.New(d.ClientID())
	}
	return &Client{
		opts:           opts,
		doc:            d,
		awareness:      a,
		dirtyDoc:       make(chan struct{}, 1),
		dirtyAwareness: make(chan struct{}, 1),
		dirtyPersist:   make(chan struct{}, 1),
	}, nil
}

// Doc returns the local document this client syncs.
func (c *Client) Doc() *doc.Doc { return c.doc }

// Awareness returns the awareness instance this client exchanges.
func (c *Client) Awareness() *awareness.Awareness { return c.awareness }

// Synced reports whether the last handshake completed and the
// connection is still up.
func (c *Client) Synced() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.synced
}

// Connect starts the connection loop and returns immediately. The
// loop dials, handshakes, relays updates, and reconnects with backoff
// until ctx is cancelled or Close is called. Calling Connect twice is
// an error.
func (c *Client) Connect(ctx context.Context) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return errors.New("client: closed")
	}
	if c.cancel != nil {
		c.mu.Unlock()
		return errors.New("client: already connected")
	}
	c.mu.Unlock()

	runCtx, cancel := context.WithCancel(ctx)

	// Offline-first: load the persisted document state BEFORE installing
	// the observer (so the replay does not re-persist) and before the
	// first dial (so the handshake state vector already covers any edits
	// made while offline, sending them up to the server). Done outside
	// c.mu because loadLocal may invoke the user's OnError callback.
	if c.opts.LocalStore != nil {
		c.loadLocal(runCtx)
	}

	// Observe transactions. Every change that touches the document is
	// persisted locally (local edits AND applied remote updates), so the
	// on-device copy stays current. Only LOCAL edits (Origin != c) mark
	// the doc dirty for broadcast; remote applies must not echo back.
	unsubscribe := c.doc.OnAfterTransaction(func(t *doc.TransactionMut) {
		if !txnChanged(t) {
			return
		}
		if c.opts.LocalStore != nil {
			select {
			case c.dirtyPersist <- struct{}{}:
			default:
			}
		}
		if t.Origin == any(c) {
			return
		}
		select {
		case c.dirtyDoc <- struct{}{}:
		default:
		}
	})

	// Publish every lifecycle handle and start the loops atomically under
	// c.mu, so a concurrent Close cannot interleave between installing the
	// observer and starting the goroutines that close done/persistDone —
	// which would race the fields and leak the observer + goroutines. If
	// Close (or a second Connect) won the race during the setup above,
	// undo what we built and abort.
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		unsubscribe()
		cancel()
		return errors.New("client: closed")
	}
	if c.cancel != nil {
		c.mu.Unlock()
		unsubscribe()
		cancel()
		return errors.New("client: already connected")
	}
	c.cancel = cancel
	c.done = make(chan struct{})
	c.unsubscribe = unsubscribe
	if c.opts.LocalStore != nil {
		c.persistDone = make(chan struct{})
		go c.persistLoop(runCtx)
	}
	go c.run(runCtx)
	c.mu.Unlock()
	return nil
}

// Close stops the connection loop, removes the change observer, and
// closes the connection. Safe to call more than once and from multiple
// goroutines; the teardown runs exactly once.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	cancel := c.cancel
	done := c.done
	persistDone := c.persistDone
	unsubscribe := c.unsubscribe
	c.unsubscribe = nil
	c.mu.Unlock()

	if cancel == nil {
		// Close arrived before Connect published its handles. closed is now
		// latched, so Connect's atomic publish will see it and abort; there
		// is nothing started here to tear down.
		return nil
	}

	// Remove the observer first so no late edit signals a stopped
	// persist loop; the loop's final persistNow reads live committed
	// state, so edits made just before Close are still captured. Then
	// stop the loops and wait for the final flush. The waits run
	// OUTSIDE c.mu because persistNow takes c.mu.
	if unsubscribe != nil {
		unsubscribe()
	}
	cancel()
	<-done
	if persistDone != nil {
		<-persistDone
	}
	return nil
}

// SetAwarenessState sets the local awareness state (a JSON blob, the
// same convention y-protocols uses) and broadcasts it.
func (c *Client) SetAwarenessState(jsonState []byte) {
	c.awareness.SetLocalState(jsonState)
	c.markAwarenessDirty()
}

// RemoveAwarenessState clears the local awareness state (peers see
// the client leave) and broadcasts the removal.
func (c *Client) RemoveAwarenessState() {
	c.awareness.RemoveLocalState()
	c.markAwarenessDirty()
}

func (c *Client) markAwarenessDirty() {
	select {
	case c.dirtyAwareness <- struct{}{}:
	default:
	}
}

// run is the reconnect loop: one attempt per iteration, exponential
// backoff between failures, reset on a successful handshake.
func (c *Client) run(ctx context.Context) {
	defer close(c.done)
	backoff := c.opts.MinBackoff
	for {
		err := c.session(ctx)
		c.setSynced(false)
		if ctx.Err() != nil {
			return
		}
		if err != nil && c.opts.OnError != nil {
			c.opts.OnError(err)
		}
		// Jittered sleep, then grow the backoff.
		sleep := backoff/2 + time.Duration(rand.Int63n(int64(backoff/2+1)))
		select {
		case <-ctx.Done():
			return
		case <-time.After(sleep):
		}
		if backoff *= 2; backoff > c.opts.MaxBackoff {
			backoff = c.opts.MaxBackoff
		}
	}
}

// session runs one connection: dial, handshake, then concurrent read
// and write pumps until either fails or ctx is cancelled.
func (c *Client) session(ctx context.Context) error {
	dialURL := strings.TrimRight(c.opts.URL, "/") + "/" + c.opts.DocName
	conn, _, err := websocket.Dial(ctx, dialURL, nil)
	if err != nil {
		return fmt.Errorf("client: dial %s: %w", dialURL, err)
	}
	conn.SetReadLimit(-1)
	sessCtx, sessCancel := context.WithCancel(ctx)
	defer sessCancel()
	defer conn.Close(websocket.StatusNormalClosure, "client closing")

	c.mu.Lock()
	c.conn = conn
	c.connCtx = sessCtx
	c.mu.Unlock()

	// Client side of the y-protocols handshake: announce our state
	// vector, ask for awareness, and push our awareness state.
	if err := c.sendStep1(); err != nil {
		return err
	}
	_ = c.write(syncpkg.EncodeQueryAwareness())
	c.sendAwareness()

	writeErr := make(chan error, 1)
	go func() { writeErr <- c.writePump(sessCtx) }()

	readErr := c.readPump(sessCtx)
	sessCancel()
	<-writeErr
	return readErr
}

// readPump applies inbound frames until the connection drops.
func (c *Client) readPump(ctx context.Context) error {
	for {
		_, raw, err := c.conn.Read(ctx)
		if err != nil {
			return fmt.Errorf("client: read: %w", err)
		}
		frame, _, err := syncpkg.DecodeEnvelope(raw)
		if err != nil {
			continue // ignore malformed frames, matching server leniency
		}
		switch frame.Type {
		case syncpkg.MessageSync, syncpkg.MessageSyncReply:
			if err := c.handleSync(frame); err != nil && c.opts.OnError != nil {
				c.opts.OnError(err)
			}
		case syncpkg.MessageAwareness:
			_, _ = c.awareness.Apply(frame.Payload, "remote")
		case syncpkg.MessageQueryAwareness:
			c.sendAwareness()
		case syncpkg.MessagePing:
			_ = c.write(syncpkg.EncodePong())
		}
	}
}

// handleSync dispatches one sync sub-message.
func (c *Client) handleSync(frame *syncpkg.Frame) error {
	switch frame.SyncSub {
	case syncpkg.SyncStep1:
		// Server announces its state vector; reply with the diff and
		// record that the server now has everything up to our SV.
		remoteSV, _, err := encoding.DecodeStateVector(frame.Payload)
		if err != nil {
			return fmt.Errorf("client: step1 decode: %w", err)
		}
		rtxn := c.doc.ReadTxn()
		diff := encoding.EncodeDiff(c.doc, rtxn, remoteSV)
		localSV := cloneSV(rtxn.Store().GetStateVector())
		rtxn.Close()
		if err := c.write(syncpkg.EncodeSyncStep2(diff)); err != nil {
			return err
		}
		c.mu.Lock()
		c.lastSent = localSV
		c.mu.Unlock()
		return nil
	case syncpkg.SyncStep2, syncpkg.SyncUpdate:
		if len(frame.Payload) == 0 {
			return nil
		}
		if err := c.apply(frame.Payload); err != nil {
			return err
		}
		if frame.SyncSub == syncpkg.SyncStep2 {
			c.setSynced(true)
		}
		return nil
	}
	return nil
}

// apply integrates a remote update, tagging the transaction with this
// client as Origin so the local-edit observer skips it.
func (c *Client) apply(payload []byte) error {
	upd, _, err := encoding.DecodeUpdate(payload)
	if err != nil {
		return fmt.Errorf("client: update decode: %w", err)
	}
	txn := c.doc.WriteTxn()
	txn.Origin = c
	defer txn.Commit()
	return upd.Apply(txn)
}

// writePump flushes local doc and awareness changes to the server
// until the session ends.
func (c *Client) writePump(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.dirtyDoc:
			c.mu.Lock()
			since := c.lastSent
			c.mu.Unlock()
			rtxn := c.doc.ReadTxn()
			diff := encoding.EncodeDiff(c.doc, rtxn, since)
			localSV := cloneSV(rtxn.Store().GetStateVector())
			rtxn.Close()
			if err := c.write(syncpkg.EncodeSyncUpdate(diff)); err != nil {
				return err
			}
			c.mu.Lock()
			c.lastSent = localSV
			c.mu.Unlock()
		case <-c.dirtyAwareness:
			c.sendAwareness()
		}
	}
}

// sendStep1 announces the local state vector.
func (c *Client) sendStep1() error {
	rtxn := c.doc.ReadTxn()
	sv := encoding.EncodeStateVector(rtxn.Store().GetStateVector(), nil)
	rtxn.Close()
	return c.write(syncpkg.EncodeSyncStep1(sv))
}

// sendAwareness pushes the local client's awareness entry.
func (c *Client) sendAwareness() {
	blob := c.awareness.Encode([]uint64{c.awareness.ClientID()})
	if len(blob) == 0 {
		return
	}
	_ = c.write(syncpkg.EncodeAwareness(blob))
}

// write sends one envelope over the current connection, if any.
func (c *Client) write(envelope []byte) error {
	c.mu.Lock()
	conn, ctx := c.conn, c.connCtx
	c.mu.Unlock()
	if conn == nil {
		return errors.New("client: not connected")
	}
	return conn.Write(ctx, websocket.MessageBinary, envelope)
}

func (c *Client) setSynced(v bool) {
	c.mu.Lock()
	changed := c.synced != v
	c.synced = v
	c.mu.Unlock()
	if changed && c.opts.OnSynced != nil {
		c.opts.OnSynced(v)
	}
}

// txnChanged reports whether a transaction produced observable
// changes: new clocks or tombstones.
func txnChanged(t *doc.TransactionMut) bool {
	if len(t.DeletedIDs()) > 0 {
		return true
	}
	before, after := t.BeforeState(), t.AfterState()
	for cid, clock := range after {
		if before[cid] != clock {
			return true
		}
	}
	return false
}

func cloneSV(sv store.StateVector) store.StateVector {
	out := make(store.StateVector, len(sv))
	for k, v := range sv {
		out[k] = v
	}
	return out
}
