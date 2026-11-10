// Package client is a Yjs sync provider over WebSocket: the Go
// equivalent of y-websocket's WebsocketProvider, for yserve and the
// reference y-websocket server. It does not speak Hocuspocus's framing,
// which prefixes every frame with the document name.
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
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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
	//
	// OnError and OnSynced may be invoked concurrently from different
	// goroutines (e.g. a local-store write failure overlapping a dial
	// failure), so a callback that mutates shared state must guard it.
	OnError func(err error)

	// MinBackoff / MaxBackoff bound the reconnect backoff. Defaults:
	// 250ms / 10s.
	MinBackoff time.Duration
	MaxBackoff time.Duration

	// AwarenessTimeout is how long peers keep this client's presence
	// without hearing from it. The client renews its state at half of
	// this, checking every tenth of it (at least every 100ms), as
	// y-protocols does. Default 30s, the y-protocols value. Set it to the
	// server's timeout when that is shorter. Do not set it above 30s when
	// JavaScript peers are in the room: y-protocols times out a client it
	// has not heard from for 30 seconds, whatever the server does.
	AwarenessTimeout time.Duration

	// LivenessTimeout bounds how long a connection that died without a
	// FIN or RST (a phone that changed networks, a NAT that dropped the
	// mapping, a server that hangs) goes unnoticed. The client drops the
	// connection and reconnects, as y-websocket does after 30 seconds
	// without a message, once nothing came from the server for this long,
	// nothing it wrote made progress, and a WebSocket ping sent after half
	// of it got no pong. OnError then gets ErrServerSilent, or the read
	// error when the ping's own write blocked for 5 seconds and the
	// WebSocket library closed the connection first. While the client is
	// busy with a frame it read (a large update applying, a callback that
	// blocks), the check waits, since what the server sent meanwhile lies
	// unread; a write the kernel takes nothing of for the whole timeout
	// drops the connection all the same.
	//
	// Progress is counted in bytes on the TCP connection, so a transfer
	// slower than the timeout is not cut. On the write side it is the
	// kernel taking bytes into the socket's send buffer, which a dead path
	// does too until the buffer is full: a client that keeps writing is
	// dropped only after that. Pings go out whenever nothing was read for
	// half the timeout, in a quiet room about every 15 seconds.
	//
	// Default 30s; a shorter value below 1s is raised to 1s. A negative
	// value turns the check off, unlike the other durations here, where a
	// negative value means the default. With the check on, the dial is
	// bounded by the timeout too. The check is also off on js/wasm, where
	// the browser's WebSocket has no ping, and when http.DefaultTransport
	// is not an *http.Transport: the client counts bytes on the
	// connections of a clone of it, dialed with its dialers. A server that
	// answers no pings and sends nothing for the timeout is reconnected to
	// each time.
	LivenessTimeout time.Duration

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

// ErrServerSilent is reported through OnError when the client dropped a
// connection that stayed silent for LivenessTimeout.
var ErrServerSilent = errors.New("client: nothing read, no write progress and no pong within LivenessTimeout")

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

	// pongs counts the liveness check's pings that got their pong;
	// liveChecks the sessions dialed with the check on.
	pongs      atomic.Int64
	liveChecks atomic.Int64
}

// defaultLivenessTimeout is y-websocket's messageReconnectTimeout. Below
// minLivenessTimeout the check's 100ms tick is too coarse: a pong read
// just after a tick is already half the timeout old at the next one.
const (
	defaultLivenessTimeout = 30 * time.Second
	minLivenessTimeout     = time.Second
)

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
	if opts.AwarenessTimeout <= 0 {
		opts.AwarenessTimeout = awareness.DefaultTimeout
	}
	if opts.LivenessTimeout == 0 {
		opts.LivenessTimeout = defaultLivenessTimeout
	}
	if opts.LivenessTimeout > 0 && opts.LivenessTimeout < minLivenessTimeout {
		opts.LivenessTimeout = minLivenessTimeout
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
		opened, err := c.session(ctx)
		// Reset the backoff when the session that just ended actually
		// completed a handshake: a transient drop after healthy
		// connectivity should redial fast, not at the grown backoff left
		// over from earlier dial failures. Checked before setSynced(false)
		// clears the flag.
		if c.Synced() {
			backoff = c.opts.MinBackoff
		}
		c.setSynced(false)
		// Peers' presence goes with a connection that was open, after
		// synced turns false, as y-websocket drops it: the next handshake
		// brings the states of the peers online then, and a peer that left
		// in between would otherwise stay. A failed dial leaves the
		// awareness alone, as y-websocket does.
		if opened {
			c.awareness.ForgetRemote()
		}
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
// and write pumps until either fails or ctx is cancelled. opened reports
// whether the dial succeeded.
func (c *Client) session(ctx context.Context) (opened bool, err error) {
	dialURL := strings.TrimRight(c.opts.URL, "/") + "/" + c.opts.DocName
	conn, live, err := c.dial(ctx, dialURL)
	if err != nil {
		return false, fmt.Errorf("client: dial %s: %w", dialURL, err)
	}
	conn.SetReadLimit(-1)
	sessCtx, sessCancel := context.WithCancel(ctx)
	defer sessCancel()
	defer conn.Close(websocket.StatusNormalClosure, "client closing")

	c.mu.Lock()
	c.conn = conn
	c.connCtx = sessCtx
	c.mu.Unlock()
	// Clear the per-session conn when the session ends, so a stale closed
	// conn is never visible between sessions (write() then guards
	// conn==nil as "not connected").
	defer func() {
		c.mu.Lock()
		c.conn = nil
		c.connCtx = nil
		c.mu.Unlock()
	}()

	// Client side of the y-protocols handshake: announce our state
	// vector, ask for awareness, and push our awareness state if there is
	// one, as y-websocket does. The state goes out one clock up: the
	// server keeps the clock of the state it removed when the last
	// session ended, and ignores a state at that clock.
	if err := c.sendStep1(); err != nil {
		return true, err
	}
	_ = c.write(syncpkg.EncodeQueryAwareness())
	if c.awareness.RenewLocalState(0) {
		c.sendAwareness()
	}

	writeErr := make(chan error, 1)
	go func() { writeErr <- c.writePump(sessCtx) }()
	var silent chan bool
	if live != nil {
		silent = make(chan bool, 1)
		go func() { silent <- c.watch(sessCtx, conn, live) }()
	}

	readErr := c.readPump(sessCtx, conn, live)
	sessCancel()
	<-writeErr
	if silent != nil && <-silent {
		return true, fmt.Errorf("%w (%s)", ErrServerSilent, c.opts.LivenessTimeout)
	}
	return true, readErr
}

// readPump applies inbound frames until the connection drops. conn is
// passed explicitly (not read from c.conn) so the read path never
// touches the lock-guarded field without the lock.
func (c *Client) readPump(ctx context.Context, conn *websocket.Conn, live *liveness) error {
	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			return fmt.Errorf("client: read: %w", err)
		}
		live.startFrame()
		c.handleFrame(raw)
		live.endFrame()
	}
}

// handleFrame applies one frame the read loop read.
func (c *Client) handleFrame(raw []byte) {
	frame, _, err := syncpkg.DecodeEnvelope(raw)
	if err != nil {
		return // ignore malformed frames, matching server leniency
	}
	switch frame.Type {
	case syncpkg.MessageSync, syncpkg.MessageSyncReply:
		if err := c.handleSync(frame); err != nil && c.opts.OnError != nil {
			c.opts.OnError(err)
		}
	case syncpkg.MessageAwareness:
		// A removal of this client while its state is set (the server
		// ending an older connection of it after this one began) makes
		// Apply raise the local clock past it; the state goes out again
		// at once at that clock, as y-websocket sends it, rather than at
		// the next renewal.
		sum, _ := c.awareness.Apply(frame.Payload, "remote")
		if slices.Contains(sum.Removed, c.awareness.ClientID()) {
			if _, ok := c.awareness.LocalState(); ok {
				c.sendAwareness()
			}
		}
	case syncpkg.MessageQueryAwareness:
		c.sendAwareness()
	case syncpkg.MessagePing:
		_ = c.write(syncpkg.EncodePong())
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
		c.raiseLastSent(localSV)
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
// client as Origin so the local-edit observer skips it. What the update
// brought came from the server, so lastSent rises over it for every client
// but this one: the next local edit's diff then carries this client's own
// changes, not the server's own back to it. Only the update's gapless
// continuation of lastSent counts (serverCoverage); content this client got
// from elsewhere and the server may lack still goes out. lastSent rises
// before the update enters the document: writePump reads lastSent inside
// its read transaction, so a diff that sees the update sees the raise too.
func (c *Client) apply(payload []byte) error {
	upd, _, err := encoding.DecodeUpdate(payload)
	if err != nil {
		return fmt.Errorf("client: update decode: %w", err)
	}
	c.mu.Lock()
	since := c.lastSent
	c.mu.Unlock()
	c.raiseLastSent(serverCoverage(upd, since, c.doc.ClientID()))
	txn := c.doc.WriteTxn()
	txn.Origin = c
	txn.Local = false // the server's update, as a yjs provider applies it
	defer txn.Commit()
	return upd.Apply(txn)
}

// serverCoverage returns, for every client in u but self, the clock up to
// which u continues from since[client] without a gap. The server that sent
// u has every clock of that client below it, as it had every clock below
// since. A skipped range, or a first block past since, ends the coverage.
func serverCoverage(u *encoding.Update, since store.StateVector, self uint64) store.StateVector {
	out := store.StateVector{}
	for cid, blocks := range u.Blocks { // each client's blocks are clock-ascending
		if cid == self {
			continue
		}
		cur := since[cid]
		for _, b := range blocks {
			var clock, length uint64
			switch b.Kind {
			case encoding.WireBlockItem:
				clock, length = b.Item.ID.Clock, b.Item.Len
			case encoding.WireBlockGC:
				clock, length = b.ID.Clock, b.Len
			default: // a skip: the update leaves this range out
				clock = cur + 1
			}
			if clock > cur {
				break
			}
			if end := clock + length; end > cur {
				cur = end
			}
		}
		if cur > since[cid] {
			out[cid] = cur
		}
	}
	return out
}

// raiseLastSent raises lastSent to sv, entry by entry; an entry never
// goes down. It installs a new map rather than changing the old one, so a
// diff computed from an earlier lastSent outside c.mu is not raced.
func (c *Client) raiseLastSent(sv store.StateVector) {
	c.mu.Lock()
	defer c.mu.Unlock()
	next := cloneSV(c.lastSent)
	for cid, clock := range sv {
		if clock > next[cid] {
			next[cid] = clock
		}
	}
	c.lastSent = next
}

// writePump flushes local doc and awareness changes to the server
// until the session ends. It also renews the local awareness state once
// it is half the awareness timeout old, checking every tenth of the
// timeout (at least every 100ms), as y-protocols does, so the server and
// peers keep an idle client's presence.
func (c *Client) writePump(ctx context.Context) error {
	renew := time.NewTicker(max(c.opts.AwarenessTimeout/10, 100*time.Millisecond))
	defer renew.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-renew.C:
			if c.awareness.RenewLocalState(c.opts.AwarenessTimeout / 2) {
				c.sendAwareness()
			}
		case <-c.dirtyDoc:
			// lastSent is read inside the transaction (see apply).
			rtxn := c.doc.ReadTxn()
			c.mu.Lock()
			since := c.lastSent
			c.mu.Unlock()
			diff := encoding.EncodeDiff(c.doc, rtxn, since)
			localSV := cloneSV(rtxn.Store().GetStateVector())
			rtxn.Close()
			if err := c.write(syncpkg.EncodeSyncUpdate(diff)); err != nil {
				return err
			}
			c.raiseLastSent(localSV)
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
