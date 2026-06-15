// Package server implements the y-websocket / Hocuspocus-compatible
// WebSocket sync server for ygo documents.
//
// The server exposes an http.Handler that adopters mount on their
// own http.ServeMux at any path prefix. Per port-note §"Go
// translation choices" — every adopter already has an HTTP server,
// so we layer on top rather than impose our own runtime. A 30-line
// cmd/ygo-server/main.go binary wraps this for stand-alone use.
//
// Wire format compatibility: the bare y-websocket subset of the
// Hocuspocus envelope (tags 0=Sync, 1=Awareness, 3=QueryAwareness).
// Auth (tag 2), Stateless (5/6), Close (7), SyncStatus (8) are
// silently ignored — see docs/tech-debt.md. The Sync subset is
// sufficient for full interop with y-websocket clients and the
// Sync+Awareness subset of Hocuspocus clients.
//
// Per-document state lives in a map keyed by docName (the last
// path segment of the WS URL). Documents are loaded lazily on the
// first connection and evicted after the last connection closes;
// if a persist.Store is configured, every applied update is
// persisted and a final snapshot is written at eviction time.
package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/Deln0r/ygo/internal/awareness"
	"github.com/Deln0r/ygo/internal/doc"
	syncpkg "github.com/Deln0r/ygo/internal/sync"
	"github.com/Deln0r/ygo/persist"
)

// Options configures a Server. The zero value is valid: in-memory
// state only, no persistence, no auth, docName extracted as the
// last URL path segment.
type Options struct {
	// Store optionally persists every applied update keyed by
	// docName. When set, new documents load their history on first
	// connect (drain through the pending buffer if necessary).
	// When nil, documents are in-memory only and lost on the last
	// disconnect.
	Store persist.Store

	// DocNameFn extracts the docName from the WS upgrade request.
	// Defaults to last-path-segment, mirroring y-websocket's
	// req.url.slice(1).split('?')[0] rule (port-note §3). Override
	// when mounting on a complex URL scheme.
	DocNameFn func(r *http.Request) string

	// OriginPatterns lists the allowed Origin headers for CORS-
	// style WS upgrade rejection. Defaults to an empty list which
	// rejects all browser cross-origin connections; pass "*" to
	// allow any origin (development only — relaxes browser
	// same-origin protection). Forwarded verbatim to coder/websocket
	// AcceptOptions.
	OriginPatterns []string

	// OnAuthenticate is the Hocuspocus auth callback. When set,
	// the server expects every client to send a MessageAuth(Token)
	// envelope shortly after connecting; the callback receives the
	// docName + token and returns nil to accept or error to deny.
	// On denial the server emits AuthPermissionDenied + Close and
	// closes the WS with code 4401 (CloseStatusUnauthorized).
	//
	// When nil (the bare y-websocket default), MessageAuth tokens
	// are accepted silently — the server responds with
	// AuthAuthenticated so Hocuspocus clients flip their internal
	// "authenticated" flag and proceed.
	OnAuthenticate syncpkg.AuthHandler

	// OnStateless is the Hocuspocus stateless-channel callback.
	// Receives docName + payload string for both MessageStateless
	// and MessageBroadcastStateless envelopes. Long-running work
	// should be dispatched off-thread — this runs on the conn's
	// read goroutine.
	//
	// MessageBroadcastStateless also fans out to other conns on
	// the doc regardless of whether the callback is set.
	OnStateless syncpkg.StatelessHandler

	// VersionInterval enables periodic auto-versioning: every
	// interval, each document that received updates since the last
	// sweep is captured as a named version ("auto") via
	// persist.SaveVersion. Requires Store to also implement
	// persist.VersionStore (the sqlite backend does); ignored with a
	// logged notice otherwise. Zero disables auto-versioning.
	VersionInterval time.Duration

	// KeepVersions prunes each auto-versioned document to its newest
	// N versions after every capture. Zero keeps every version
	// (history grows unbounded; prune externally).
	KeepVersions int

	// AwarenessTimeout bounds how long a silent presence entry
	// survives before the server sweeps it: any non-local awareness
	// client whose last update is older than this is marked offline
	// and the removal is broadcast to the room. The sweep also
	// garbage-collects tombstones older than twice this value,
	// reclaiming memory from churned clients. Zero selects the
	// y-protocols default (30s). Healthy clients heartbeat well
	// inside the window (y-websocket every 15s), so the sweep only
	// evicts the wedged.
	AwarenessTimeout time.Duration

	// MaxAwarenessClients bounds the distinct presence clientIDs a
	// single room tracks, capping the memory one peer can force by
	// flooding fabricated clientIDs. Zero selects a safe default
	// (4096); a negative value disables the cap. New clients beyond
	// the cap are dropped; already-tracked clients keep updating.
	MaxAwarenessClients int

	// ReadLimit sets the maximum WebSocket message size in bytes that
	// the server will read from a single client frame. Defaults to
	// coder/websocket's built-in 32 768-byte limit when zero. Raise
	// this for large collaborative documents (e.g. 128 << 20 for
	// 128 MiB). Set to -1 for unlimited.
	ReadLimit int64
}

// Server is the http.Handler implementation. Construct with New
// and mount via Handler(). Safe for concurrent use.
type Server struct {
	opts Options

	docsMu sync.Mutex
	docs   map[string]*docState

	// Auto-versioning state (see versioning.go). versionDirty holds
	// the docNames updated since the last sweep; the stop/done pair
	// manages the ticker goroutine's lifecycle.
	versionMu    sync.Mutex
	versionDirty map[string]struct{}
	versionStop  chan struct{}
	versionDone  chan struct{}

	// Awareness-sweep state (see awareness_sweep.go). The stop/done
	// pair manages the eviction ticker goroutine's lifecycle.
	awarenessStop chan struct{}
	awarenessDone chan struct{}
}

// Default awareness-hardening parameters, applied to the zero value
// of the matching Options fields. The timeout matches y-protocols'
// outdatedTimeout; the client cap is generous for any real room while
// bounding a flood.
const (
	defaultAwarenessTimeout    = 30 * time.Second
	defaultMaxAwarenessClients = 4096
)

// New returns a Server with the given options. The returned Server
// is ready to accept WS connections; call Handler() to obtain the
// http.Handler that performs the upgrade.
func New(opts Options) *Server {
	if opts.DocNameFn == nil {
		opts.DocNameFn = defaultDocName
	}
	if opts.AwarenessTimeout == 0 {
		opts.AwarenessTimeout = defaultAwarenessTimeout
	}
	if opts.MaxAwarenessClients == 0 {
		opts.MaxAwarenessClients = defaultMaxAwarenessClients
	}
	s := &Server{
		opts: opts,
		docs: map[string]*docState{},
	}
	s.startVersioning()
	s.startAwarenessSweep()
	return s
}

// Handler returns the http.Handler that performs the WebSocket
// upgrade and routes the resulting connection through the sync
// state machine. Mount it on a mux pattern such as "/" or
// "/collab/{docName}".
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(s.serveWS)
}

// Close evicts every in-memory document, calling Flush on the
// configured Store. Pending in-flight WS reads will fail with
// context cancellation; callers should drain via an http.Server
// Shutdown rather than Close in production.
//
// Returns the first error encountered while flushing, but
// continues attempting eviction past errors so partial failure
// leaves no leaks.
func (s *Server) Close(ctx context.Context) error {
	s.stopVersioning()
	s.stopAwarenessSweep()
	s.docsMu.Lock()
	names := make([]string, 0, len(s.docs))
	for name := range s.docs {
		names = append(names, name)
	}
	s.docsMu.Unlock()

	var firstErr error
	for _, name := range names {
		if s.opts.Store != nil {
			if err := s.opts.Store.Flush(ctx, name); err != nil && firstErr == nil {
				firstErr = fmt.Errorf("flush %q: %w", name, err)
			}
		}
	}
	return firstErr
}

// defaultDocName implements the y-websocket path-strip convention.
func defaultDocName(r *http.Request) string {
	p := strings.TrimPrefix(r.URL.Path, "/")
	if i := strings.Index(p, "/"); i >= 0 {
		p = p[i+1:]
	}
	return p
}

func (s *Server) serveWS(w http.ResponseWriter, r *http.Request) {
	docName := s.opts.DocNameFn(r)
	if docName == "" {
		http.Error(w, "empty docName", http.StatusBadRequest)
		return
	}

	wsConn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: s.opts.OriginPatterns,
	})
	if err != nil {
		// websocket.Accept already wrote a response.
		return
	}
	if s.opts.ReadLimit != 0 {
		wsConn.SetReadLimit(s.opts.ReadLimit)
	}

	state, err := s.acquireDoc(r.Context(), docName)
	if err != nil {
		_ = wsConn.Close(websocket.StatusInternalError,
			fmt.Sprintf("load doc: %v", err))
		return
	}

	c := s.newConn(state, wsConn)
	state.addConn(c)
	defer s.releaseConn(r.Context(), state, c)

	if err := c.handler.SendInitialSync(); err != nil {
		return
	}

	c.readLoop(r.Context())
}

// docState carries the Doc + Awareness + connection set for one
// docName. Created lazily by acquireDoc; freed by releaseConn when
// the last connection departs.
type docState struct {
	name      string
	doc       *doc.Doc
	awareness *awareness.Awareness

	connsMu sync.RWMutex
	conns   map[*conn]struct{}
}

// acquireDoc returns the docState for docName, creating it (and
// loading from Store, if configured) on first request. The caller
// is responsible for calling releaseConn after the connection
// closes — this is the reference-count "increment" half.
func (s *Server) acquireDoc(ctx context.Context, docName string) (*docState, error) {
	s.docsMu.Lock()
	defer s.docsMu.Unlock()
	if state, ok := s.docs[docName]; ok {
		return state, nil
	}

	var d *doc.Doc
	if s.opts.Store != nil {
		loaded, err := persist.LoadDoc(ctx, s.opts.Store, docName, doc.Options{})
		if err != nil {
			return nil, err
		}
		d = loaded
	} else {
		d = doc.NewDoc()
	}

	aw := awareness.New(d.ClientID())
	aw.SetMaxClients(s.opts.MaxAwarenessClients)
	state := &docState{
		name:      docName,
		doc:       d,
		awareness: aw,
		conns:     map[*conn]struct{}{},
	}
	s.docs[docName] = state
	return state, nil
}

// releaseConn removes a connection from the docState's set, calls
// the sync handler's Disconnect (which tombstones controlled
// awareness clients), and — when the connection set hits zero —
// evicts the docState from the registry, optionally flushing to
// Store.
func (s *Server) releaseConn(ctx context.Context, state *docState, c *conn) {
	state.connsMu.Lock()
	delete(state.conns, c)
	remaining := len(state.conns)
	state.connsMu.Unlock()

	tombstoned := c.handler.Disconnect()
	if len(tombstoned) > 0 {
		// Broadcast the resulting awareness removals to remaining
		// peers so they learn this peer's clients departed.
		state.broadcastAwarenessRemovals(tombstoned)
	}

	if remaining > 0 {
		return
	}

	s.docsMu.Lock()
	delete(s.docs, state.name)
	s.docsMu.Unlock()

	if s.opts.Store != nil {
		// Flush is best-effort — log via context cancellation if
		// the server is shutting down. We do not block on errors;
		// the document log is intact in the Store either way.
		_ = s.opts.Store.Flush(ctx, state.name)
	}
}

// addConn registers a connection in the docState's set. The
// connection's broadcast callback fan-outs to this set.
func (s *docState) addConn(c *conn) {
	s.connsMu.Lock()
	s.conns[c] = struct{}{}
	s.connsMu.Unlock()
}

// snapshotConns returns a slice copy of current connections, safe
// for iteration after the lock releases.
func (s *docState) snapshotConns() []*conn {
	s.connsMu.RLock()
	defer s.connsMu.RUnlock()
	out := make([]*conn, 0, len(s.conns))
	for c := range s.conns {
		out = append(out, c)
	}
	return out
}

// conn is the per-WebSocket connection wrapper that owns the
// sync.Conn state machine and the websocket.Conn write mutex.
type conn struct {
	server   *Server
	state    *docState
	ws       *websocket.Conn
	handler  *syncpkg.Conn
	writeMu  sync.Mutex
	idForLog string
}

var connIDCounter atomic.Uint64

// newConn builds a conn wired to the server and docState. The
// handler's Send and Broadcast callbacks reference back into the
// conn so the WS transport stays encapsulated.
func (s *Server) newConn(state *docState, ws *websocket.Conn) *conn {
	id := fmt.Sprintf("ws-%d", connIDCounter.Add(1))
	c := &conn{
		server:   s,
		state:    state,
		ws:       ws,
		idForLog: id,
	}
	h := syncpkg.New(state.doc, state.awareness, id)
	h.Send = c.send
	h.Broadcast = c.broadcast
	h.DocName = state.name
	h.OnAuthenticate = s.opts.OnAuthenticate
	h.OnStateless = s.opts.OnStateless
	c.handler = h
	return c
}

// send writes one envelope to the underlying WS. The writeMu
// serializes concurrent writes (a broadcast on a peer's goroutine
// can race with a Send on this conn's read goroutine).
func (c *conn) send(envelope []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.ws.Write(context.Background(), websocket.MessageBinary, envelope)
}

// broadcast fans an envelope to every connection on the same doc.
// Self-included per port-note gotcha 6 — V1 updates are idempotent
// so receivers tolerate the echo. Failures on individual peers are
// logged-and-skipped; we do NOT propagate them back to the
// originator.
func (c *conn) broadcast(envelope []byte) {
	for _, peer := range c.state.snapshotConns() {
		_ = peer.send(envelope)
	}
	// Persist sync updates to the store, if configured. We dispatch
	// here rather than inside the sync handler so the persistence
	// concern stays in the transport layer.
	if c.server == nil {
		return
	}
	c.maybePersist(envelope)
}

// broadcastAwarenessRemovals fans out tombstone envelopes for the
// given clientIDs to every connection on the document. Called both
// after Disconnect when a conn departs and from the periodic sweep
// (which has no originating conn); peers learn the clients are gone
// via a normal awareness frame carrying "null" state.
func (s *docState) broadcastAwarenessRemovals(ids []uint64) {
	if len(ids) == 0 {
		return
	}
	// Encode the now-removed entries (the underlying ClientState
	// retained the clock after RemoveState / SweepOutdated, so Encode
	// emits a proper "null" sentinel for each).
	wire := s.awareness.Encode(ids)
	envelope := syncpkg.EncodeAwareness(wire)
	for _, peer := range s.snapshotConns() {
		_ = peer.send(envelope)
	}
}

// maybePersist appends a SyncUpdate's inner update bytes to the
// store. Awareness frames and SyncStep1 are not persisted (they
// are ephemeral / handshake-only). SyncStep2 IS persisted because
// it carries content the server didn't have before this connect.
func (c *conn) maybePersist(envelope []byte) {
	if c.server.opts.Store == nil {
		return
	}
	frame, _, err := syncpkg.DecodeEnvelope(envelope)
	if err != nil {
		return
	}
	if frame.Type != syncpkg.MessageSync {
		return
	}
	if frame.SyncSub != syncpkg.SyncStep2 && frame.SyncSub != syncpkg.SyncUpdate {
		return
	}
	if len(frame.Payload) == 0 {
		return
	}
	if err := c.server.opts.Store.StoreUpdate(context.Background(), c.state.name, frame.Payload); err != nil {
		// A failed persist must not be invisible, and must not mark the
		// document dirty: auto-versioning would then capture in-memory
		// state that was never durably stored. Log and skip.
		log.Printf("server: persist update for %q: %v", c.state.name, err)
		return
	}
	c.server.markVersionDirty(c.state.name)
}

// readLoop runs the per-connection message-receive loop until the
// underlying WS closes or a fatal protocol error surfaces. The
// caller (serveWS) is responsible for cleanup via the deferred
// releaseConn.
func (c *conn) readLoop(ctx context.Context) {
	for {
		_, raw, err := c.ws.Read(ctx)
		if err != nil {
			return
		}
		frame, _, err := syncpkg.DecodeEnvelope(raw)
		if err != nil {
			// Malformed frame — close with a protocol-error reason.
			_ = c.ws.Close(websocket.StatusProtocolError,
				fmt.Sprintf("decode envelope: %v", err))
			return
		}
		if err := c.handler.HandleFrame(frame); err != nil {
			// Application-layer errors (apply failure, encode
			// failure) close the connection — the doc state is
			// preserved for the next reconnect.
			_ = c.ws.Close(websocket.StatusInternalError,
				fmt.Sprintf("handle frame: %v", err))
			return
		}
		// Hocuspocus auth: the handler sets AuthFailed when it has
		// sent AuthPermissionDenied + Close envelopes; the
		// transport tears down with the reserved 4401 code so
		// Hocuspocus clients see "unauthorized" rather than a
		// generic disconnect.
		if c.handler.AuthFailed {
			_ = c.ws.Close(syncpkg.CloseStatusUnauthorized, "unauthorized")
			return
		}
	}
}

// ErrServerClosed is returned from operations on a Server that has
// been Closed. Reserved for future use; currently no method returns
// it.
var ErrServerClosed = errors.New("server: closed")
