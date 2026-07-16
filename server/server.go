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
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/Deln0r/ygo/internal/awareness"
	"github.com/Deln0r/ygo/internal/doc"
	"github.com/Deln0r/ygo/internal/encoding"
	syncpkg "github.com/Deln0r/ygo/internal/sync"
	"github.com/Deln0r/ygo/persist"
	"github.com/Deln0r/ygo/server/backplane"
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

	// OnConnect, when set, is called once a connection has been admitted
	// to its room (past the per-doc and global caps) but before the
	// initial sync is sent. It receives a stable per-connection id
	// (unique for the server's lifetime), the docName, and the original
	// upgrade *http.Request, so an adopter can gate on headers, cookies,
	// TLS state, or remote address — request context that the token-only
	// OnAuthenticate does not see. Returning a non-nil error rejects the
	// connection: it is torn down and the WS is closed with
	// StatusPolicyViolation, and OnDisconnect does NOT fire for it. A
	// panic inside OnConnect is also torn down cleanly (the room slot is
	// released, no leak) and likewise does not fire OnDisconnect. This
	// runs on the connection's serve goroutine (so it runs concurrently
	// across connections); keep it fast.
	OnConnect func(connID, docName string, r *http.Request) error

	// OnDisconnect, when set, is called when a connection that OnConnect
	// accepted (or any admitted connection, if OnConnect is nil) is
	// released, with the same connID and docName passed to OnConnect.
	// It pairs one-to-one with a successful admission, so an adopter can
	// keep a balanced per-connection ledger. It runs on the connection's
	// serve goroutine after the room teardown; keep it fast.
	OnDisconnect func(connID, docName string)

	// ReadOnly, when set and it returns true for a connection, marks
	// that connection read-only: the server drops any document update it
	// sends (the update reaches neither the document nor other peers),
	// while still delivering updates to it and accepting its awareness
	// (presence). Decide from the request the way OnConnect gates
	// admission (a JWT claim, header, or path). A read-only client
	// should also disable editing in its UI; the server enforcement is
	// the backstop. Nil means every connection is read-write.
	ReadOnly func(docName string, r *http.Request) bool

	// OnChange, when set, is called after a document update is applied
	// and broadcast, with the docName and the V1 update bytes that were
	// applied. Use it for side effects on edits (search indexing,
	// webhooks, audit, external persistence). It fires for every accepted
	// document mutation whether or not a Store is configured, and NOT for
	// an update a read-only connection sent (that is dropped before
	// broadcast) nor for awareness or handshake frames. It runs
	// synchronously on the originating connection's read goroutine, so
	// keep it fast and dispatch long-running work elsewhere; a panic
	// tears down that connection. When a Store is also configured,
	// OnChange runs after the StoreUpdate attempt and fires even if that
	// StoreUpdate failed (the document already changed in memory and on
	// peers; the failure is logged), so do not treat an OnChange call as
	// confirmation of durable persistence. Treat the update slice as
	// valid only for the duration of the call; copy it if you retain it.
	OnChange func(docName string, update []byte)

	// OnLoadDocument, when set, is called the first time a document is
	// loaded into memory (a cache miss), after any Store history has been
	// applied, with the docName. It lets an adopter seed or load initial
	// content from a source other than the Store. A returned V1 update is
	// applied to the fresh document; a returned error aborts the load and
	// the connection is closed with StatusInternalError, leaving no
	// document registered.
	//
	// The returned update is applied in memory and is not itself
	// persisted, so it re-applies on every first load (including after an
	// eviction). For that to be safe the seed MUST be deterministic with
	// STABLE item IDs across calls — build it from a fixed reserved
	// ClientID with fixed clocks, or return the same bytes each time. A
	// seed built from a fresh/random ClientID each call (the default of
	// doc.NewDoc) will, together with a Store, both duplicate its content
	// on every reload and strand persisted client edits that referenced
	// the seed (their origin never reappears). Prefer the Store for
	// durable content; use this hook for stable, regenerable seeds.
	//
	// It runs synchronously under the internal document-registry lock, so
	// it must be fast and MUST NOT call back into the Server (Stats,
	// Close, or opening another connection) or it will deadlock. While it
	// runs, all connection admission and document eviction across the
	// server are stalled, not just this document's creation.
	OnLoadDocument func(docName string) ([]byte, error)

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

	// WriteTimeout bounds a single broadcast/fan-out write to one peer.
	// Without it a slow or half-open consumer (full TCP send buffer, no
	// FIN) blocks the write forever, freezing the originating read loop
	// and the global awareness sweep for every other document. On
	// timeout the offending peer is closed and dropped. Zero selects a
	// safe default (10s).
	WriteTimeout time.Duration

	// MaxConnsPerDoc bounds the number of simultaneous WebSocket
	// connections to a single document, capping the goroutines and
	// memory one peer can pin by opening many sockets to the same room.
	// Zero selects a safe default (4096); a negative value disables the
	// cap. A connection beyond the cap is rejected at the upgrade with
	// websocket.StatusPolicyViolation; connections already in the room
	// keep working. The newest socket is the one refused, so an
	// established collaborator is never displaced by a flood.
	MaxConnsPerDoc int

	// MaxDocs bounds the number of distinct documents the server holds
	// in memory at once, capping the docStates (and Store loads) a peer
	// can force by connecting to many fabricated docNames. Unlike the
	// per-room caps above, this defaults to UNLIMITED (zero or negative):
	// a global document count scales with deployment size, so a default
	// cap would silently break a large multi-tenant server. Operators
	// fronting untrusted clients should set a positive bound. Documents
	// evict when their last connection departs, so the cap tracks
	// concurrently-active rooms, not lifetime document count. A
	// connection that would create a document past the cap is refused at
	// the upgrade with websocket.StatusTryAgainLater (1013); an already
	// resident room is always admitted.
	MaxDocs int

	// MaxConns bounds the total number of simultaneous WebSocket
	// connections the server holds across all documents at once: a
	// server-wide ceiling that complements the per-room MaxConnsPerDoc.
	// Like MaxDocs, it defaults to UNLIMITED (zero or negative), because a
	// total-connection count scales with deployment size and a default cap
	// would silently break a large server. Operators fronting untrusted
	// clients should set a positive bound sized to their memory and
	// file-descriptor budget. A connection that would exceed the cap is
	// refused at the upgrade with websocket.StatusTryAgainLater (1013)
	// before its room is created or loaded, so a rejected connection
	// neither orphans a fresh document nor forces a Store load; connections
	// already established keep working and the newest socket is the one
	// refused. The count is of admitted connections and drops as they
	// depart, so the cap tracks live load, not lifetime connections.
	MaxConns int

	// Backplane, when set, fans document updates between this server and
	// other instances that share it, so a horizontally-scaled deployment
	// (several servers behind a load balancer) converges on the same
	// document. For every document a server holds resident it Subscribes to
	// foreign updates (applying them to its in-memory copy and
	// re-broadcasting to its own clients) and Publishes each update its own
	// clients apply. Nil means single-instance: updates reach only the
	// clients connected to this server (plus the shared Store, if any).
	//
	// A foreign update is applied and re-broadcast locally but is NOT
	// re-persisted (the originating instance already persisted it to the
	// shared Store), does NOT fire OnChange (that fires once, on the
	// instance whose client made the edit), and is NOT re-published (which
	// would echo). Presence/awareness is per-instance and is not carried
	// over the backplane: clients see the cursors of peers on their own
	// server only.
	//
	// SHARED STORE REQUIRED. Because a foreign update is applied only in
	// memory and never re-persisted by the receiving instance, every
	// instance sharing a document MUST be backed by the SAME Store (one
	// database, not a per-instance file). The backplane carries live deltas;
	// the shared Store is the source of truth an instance loads from when a
	// document first becomes resident (and reloads after an eviction). With
	// per-instance Stores a document silently loses every cross-instance
	// edit on the next eviction+reload, and a newly-resident instance cannot
	// obtain a document another instance already edited only in memory. Do
	// not run multi-instance without a shared Store.
	//
	// BACKPRESSURE. Publish delivers to each peer instance in order and
	// never drops an update (dropping a delta would diverge a peer). A peer
	// whose local fan-out stalls (a slow or half-open client, bounded by
	// WriteTimeout) therefore applies backpressure to the publishing
	// client's edit throughput until it drains; this is bounded and
	// self-healing (the stalled client is closed on WriteTimeout) but couples
	// edit latency across instances under a wedged consumer.
	Backplane backplane.Backplane
}

// defaultWriteTimeout bounds a per-peer broadcast write when
// Options.WriteTimeout is unset.
const defaultWriteTimeout = 10 * time.Second

// Server is the http.Handler implementation. Construct with New
// and mount via Handler(). Safe for concurrent use.
type Server struct {
	opts Options

	docsMu sync.Mutex
	docs   map[string]*docState

	// totalConns counts admitted WebSocket connections across all
	// documents, backing the global Options.MaxConns cap. It is bumped
	// under docsMu in admitConn (so admissions serialize against the cap
	// check and cannot overshoot) and dropped in releaseConn; an atomic so
	// the decrement, which runs outside docsMu, is safe.
	totalConns atomic.Int64

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
	defaultMaxConnsPerDoc      = 4096
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

// Stats is a point-in-time snapshot of server load returned by Stats.
type Stats struct {
	// Documents is the number of documents resident in memory: rooms
	// with at least one live connection (a document evicts when its last
	// connection departs).
	Documents int
	// Connections is the total number of live WebSocket connections
	// across all resident documents.
	Connections int
	// Docs is the per-document breakdown, one entry per resident document,
	// sorted by Name for a stable read. Documents is len(Docs) and
	// Connections is the sum of their Connections. Ranging over it lets an
	// adopter surface hot rooms without its own accounting.
	Docs []DocStat
}

// DocStat is the per-document line of a Stats snapshot.
type DocStat struct {
	// Name is the docName of a resident document.
	Name string
	// Connections is the number of live WebSocket connections on it.
	Connections int
}

// Stats returns a point-in-time snapshot of resident documents and live
// connections, including a per-document breakdown (Stats.Docs). It is safe
// for concurrent use and cheap: it walks the document registry under the
// registry lock, taking each document's connection lock only briefly.
// Adopters typically poll it to feed their own metrics, health, or
// capacity surfaces (pair it with the OnConnect and OnDisconnect hooks for
// event-level accounting). Lock order docsMu -> connsMu matches the rest
// of the server.
func (s *Server) Stats() Stats {
	s.docsMu.Lock()
	defer s.docsMu.Unlock()
	st := Stats{Documents: len(s.docs), Docs: make([]DocStat, 0, len(s.docs))}
	for name, ds := range s.docs {
		ds.connsMu.RLock()
		n := len(ds.conns)
		ds.connsMu.RUnlock()
		st.Connections += n
		st.Docs = append(st.Docs, DocStat{Name: name, Connections: n})
	}
	sort.Slice(st.Docs, func(i, j int) bool { return st.Docs[i].Name < st.Docs[j].Name })
	return st
}

// Flush compacts docName's persisted update log into a single snapshot
// through the configured Store, forcing a durable checkpoint on demand
// without evicting the document or disturbing its live connections. It is
// the explicit form of the compaction the server already performs when a
// document is evicted or the server closes.
//
// Every applied update is persisted incrementally as it arrives, so Flush
// does not push unsaved in-memory state; it consolidates what is already
// stored, keeping the log compact and the next load fast. Call it before a
// backup, or on a schedule for hot documents.
//
// With no Store configured it is a no-op returning nil, as is a docName
// the Store has never seen. It is safe to call on a live document: a
// concurrent edit that commits mid-flush never loses data, though under
// heavy concurrent writes to the same document Flush may return a
// transient store error, in which case retry. It takes no server lock, so
// it never blocks connection admission or eviction.
func (s *Server) Flush(ctx context.Context, docName string) error {
	if s.opts.Store == nil {
		return nil
	}
	return s.opts.Store.Flush(ctx, docName)
}

// Close evicts every in-memory document, calling Flush on the
// configured Store, and closes the configured Backplane connection.
// Pending in-flight WS reads will fail with context cancellation;
// callers should drain via an http.Server Shutdown rather than Close
// in production.
//
// Returns the first error encountered while flushing or closing the
// backplane, but continues past errors so partial failure leaves no
// leaks.
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
	// Close this server's backplane connection, stopping every subscription
	// it still holds (a graceful conn drain would unsub each on eviction,
	// but Close may run before that; closing the connection is idempotent
	// and stops any that linger). The connection is this server's own; the
	// shared hub or broker outlives it.
	if s.opts.Backplane != nil {
		if err := s.opts.Backplane.Close(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("backplane close: %w", err)
		}
	}
	return firstErr
}

// maxConnsPerDoc resolves the per-document connection cap from options:
// zero selects the default, a negative option disables the cap
// (returned as -1, which addConn treats as unlimited). Resolved lazily
// per call rather than normalized in New, so Options stays a faithful
// record of caller input and the -1 unlimited sentinel is preserved.
func (s *Server) maxConnsPerDoc() int {
	switch {
	case s.opts.MaxConnsPerDoc == 0:
		return defaultMaxConnsPerDoc
	case s.opts.MaxConnsPerDoc < 0:
		return -1
	default:
		return s.opts.MaxConnsPerDoc
	}
}

// maxConns resolves the global connection cap from options: zero or
// negative means unlimited (returned as -1), mirroring MaxDocs, because a
// server-wide connection count scales with deployment size. A positive
// value is the cap. Resolved lazily per call so Options stays a faithful
// record of caller input.
func (s *Server) maxConns() int {
	if s.opts.MaxConns <= 0 {
		return -1
	}
	return s.opts.MaxConns
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

	c, state, ok, err := s.admitConn(r.Context(), docName, wsConn)
	if err != nil {
		if errors.Is(err, errServerDocsFull) {
			// Server is at its document cap. Signal transient capacity so
			// a client can back off and retry, distinct from the per-doc
			// connection cap's policy-violation close.
			_ = wsConn.Close(websocket.StatusTryAgainLater, "server document limit reached")
			return
		}
		if errors.Is(err, errServerConnsFull) {
			// Server is at its global connection cap. Transient capacity,
			// same close code as the document cap; the reason distinguishes.
			_ = wsConn.Close(websocket.StatusTryAgainLater, "server connection limit reached")
			return
		}
		_ = wsConn.Close(websocket.StatusInternalError,
			fmt.Sprintf("load doc: %v", err))
		return
	}
	if !ok {
		// Room is at its connection cap. Refuse the newest socket and
		// leave the established ones untouched. The resolved cap is
		// always >= 1 (or negative = unlimited), so a rejection means
		// the room already holds a live connection and its docState is
		// never orphaned on this path.
		_ = wsConn.Close(websocket.StatusPolicyViolation, "document connection limit reached")
		return
	}
	// Tear down on exit no matter what, including a panic inside the
	// adopter's OnConnect: register the cleanup defer BEFORE calling
	// OnConnect so releaseConn always runs and the room slot can never
	// leak. OnDisconnect fires only for a connection OnConnect accepted
	// (or any admitted connection when OnConnect is nil).
	accepted := s.opts.OnConnect == nil
	defer func() {
		s.releaseConn(r.Context(), state, c)
		if accepted && s.opts.OnDisconnect != nil {
			s.opts.OnDisconnect(c.idForLog, docName)
		}
	}()

	if s.opts.OnConnect != nil {
		// Request-aware gate, before syncing starts. A rejection (or a
		// panic) tears the connection back down via the defer above
		// without firing OnDisconnect.
		if err := s.opts.OnConnect(c.idForLog, docName, r); err != nil {
			_ = wsConn.Close(websocket.StatusPolicyViolation, "connection rejected")
			return
		}
		accepted = true
	}

	// Resolve read-only status before the read loop starts, so the
	// handler (read on its own goroutine) sees a settled flag.
	if s.opts.ReadOnly != nil {
		c.handler.ReadOnly = s.opts.ReadOnly(docName, r)
	}

	if err := c.handler.SendInitialSync(); err != nil {
		return
	}

	c.readLoop(r.Context())
}

// docState carries the Doc + Awareness + connection set for one
// docName. Created lazily by getOrCreateDocLocked; freed by releaseConn
// when the last connection departs.
type docState struct {
	name      string
	doc       *doc.Doc
	awareness *awareness.Awareness

	// backplaneUnsub stops this document's backplane subscription; nil when
	// no Backplane is configured. Set under docsMu when the document
	// becomes resident, called once when it is evicted.
	backplaneUnsub func()

	connsMu sync.RWMutex
	conns   map[*conn]struct{}
}

// errServerDocsFull is returned by getOrCreateDocLocked when admitting a
// new document would exceed Options.MaxDocs. serveWS maps it to a
// StatusTryAgainLater close so a client learns the rejection is
// transient server capacity, not a per-document policy limit.
var errServerDocsFull = errors.New("server: document limit reached")

// errServerConnsFull is returned by admitConn when admitting another
// connection would exceed Options.MaxConns, the global connection cap.
// serveWS maps it, like errServerDocsFull, to a StatusTryAgainLater close
// so a client learns the rejection is transient server capacity.
var errServerConnsFull = errors.New("server: connection limit reached")

// getOrCreateDocLocked returns the docState for docName, creating it
// (and loading from Store, if configured) on first request. The caller
// MUST hold s.docsMu. Holding docsMu across both this lookup/create and
// the connection insert (see admitConn) is what makes admission atomic
// with eviction: it is the mechanism that closes the split-room window.
func (s *Server) getOrCreateDocLocked(ctx context.Context, docName string) (*docState, error) {
	if state, ok := s.docs[docName]; ok {
		return state, nil
	}

	// Creating a new room: enforce the global document cap before any
	// (potentially expensive) Store load, so a peer at the cap cannot
	// force loads it will not be allowed to keep. Existing rooms (the
	// cache hit above) are never rejected.
	if s.opts.MaxDocs > 0 && len(s.docs) >= s.opts.MaxDocs {
		return nil, errServerDocsFull
	}

	// Subscribe to foreign updates BEFORE loading the document. Loading first
	// and subscribing after would leave a window in which an update another
	// instance publishes during our load reaches neither the not-yet-live
	// subscription nor the already-read copy, permanently diverging this
	// instance (a missing causal dependency parks every later edit from that
	// client). Subscribing first buffers any such update; it is drained into
	// the doc once loaded. Until the docState is registered, any early return
	// must release the subscription so it does not leak.
	var buf *bufferedApply
	var backplaneUnsub func()
	if s.opts.Backplane != nil {
		buf = &bufferedApply{}
		unsub, err := s.opts.Backplane.Subscribe(docName, buf.onUpdate)
		if err != nil {
			return nil, fmt.Errorf("backplane subscribe for %q: %w", docName, err)
		}
		backplaneUnsub = unsub
	}
	registered := false
	defer func() {
		if !registered && backplaneUnsub != nil {
			backplaneUnsub()
		}
	}()

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

	// OnLoadDocument seeds/loads initial content on first load, applied
	// on top of any Store history. An error aborts before registration
	// (no half-doc); a returned update is applied as V1 content.
	if s.opts.OnLoadDocument != nil {
		seed, err := s.opts.OnLoadDocument(docName)
		if err != nil {
			return nil, fmt.Errorf("OnLoadDocument(%q): %w", docName, err)
		}
		if len(seed) > 0 {
			if err := encoding.ApplyUpdate(d, seed); err != nil {
				return nil, fmt.Errorf("apply OnLoadDocument seed for %q: %w", docName, err)
			}
		}
	}

	aw := awareness.New(d.ClientID())
	aw.SetMaxClients(s.opts.MaxAwarenessClients)
	state := &docState{
		name:      docName,
		doc:       d,
		awareness: aw,
		conns:     map[*conn]struct{}{},
	}
	state.backplaneUnsub = backplaneUnsub

	// Activate the buffered subscription onto the live doc: drain any foreign
	// updates that arrived during the load, then apply directly thereafter.
	if buf != nil {
		buf.activate(func(update []byte) { s.applyBackplaneUpdate(state, update) })
	}

	s.docs[docName] = state
	registered = true
	return state, nil
}

// bufferedApply bridges a backplane subscription that is created before its
// docState's doc exists (subscribe-before-load): foreign updates arriving
// during the load are buffered, then activate replays them into the live
// apply function and switches to applying directly. It closes the
// load-before-subscribe gap where an update published while this instance
// was still loading would reach neither the not-yet-live subscription nor
// the already-read doc, permanently diverging the instance.
type bufferedApply struct {
	mu       sync.Mutex
	buffered [][]byte
	live     func([]byte)
}

// onUpdate is the subscription handler: buffer until activated, then apply
// live. The switch is atomic under mu, so every update is either buffered
// (drained later) or applied live, never both and never dropped.
func (b *bufferedApply) onUpdate(update []byte) {
	b.mu.Lock()
	if b.live == nil {
		b.buffered = append(b.buffered, append([]byte(nil), update...))
		b.mu.Unlock()
		return
	}
	live := b.live
	b.mu.Unlock()
	live(update)
}

// activate installs the live apply function and drains whatever buffered
// during the load. An update racing activate is either already buffered
// (drained here) or sees live set (applied directly); order between the two
// does not matter because Yjs integration re-drains its own pending queue,
// so at-least-once delivery is all convergence needs.
func (b *bufferedApply) activate(live func([]byte)) {
	b.mu.Lock()
	pending := b.buffered
	b.buffered = nil
	b.live = live
	b.mu.Unlock()
	for _, u := range pending {
		live(u)
	}
}

// admitConn finds or creates the docState for docName and registers a
// new connection in it, enforcing the per-document cap — all under a
// single s.docsMu hold. Holding docsMu across the find/create AND the
// insert closes the split-room TOCTOU: releaseConn evicts a docState
// only under docsMu, so a connection can never be admitted onto a state
// that is being (or has just been) removed from the registry. Doing the
// lookup (acquireDoc) and the insert (addConn) as two separate critical
// sections left exactly that window open — between the lookup returning
// and addConn running, the last existing connection could evict the
// state, orphaning the new connection onto a doc no longer in s.docs and
// splitting the room onto divergent Docs that never converge.
//
// Returns the admitted conn with its docState. When the room is at its
// cap it returns ok=false (the caller closes the socket); the resolved
// cap is always >= 1 or negative (unlimited), so a fresh zero-conn
// docState is never rejected on its first connection, and the reject
// path therefore never orphans a state. Lock order docsMu -> connsMu
// (addConn takes connsMu) matches releaseConn's eviction and is not
// inverted anywhere.
func (s *Server) admitConn(ctx context.Context, docName string, ws *websocket.Conn) (c *conn, state *docState, ok bool, err error) {
	s.docsMu.Lock()
	defer s.docsMu.Unlock()
	// Global connection cap: refuse before creating or loading a room so a
	// rejected connection neither orphans a fresh docState nor forces a
	// Store load. The check here and the matching increment below both run
	// under docsMu, so admissions serialize against the cap and can never
	// overshoot it; concurrent releases only decrement totalConns, making
	// this check at worst conservative, never permissive.
	if lim := s.maxConns(); lim > 0 && int(s.totalConns.Load()) >= lim {
		return nil, nil, false, errServerConnsFull
	}
	state, err = s.getOrCreateDocLocked(ctx, docName)
	if err != nil {
		return nil, nil, false, err
	}
	c = s.newConn(state, ws)
	if !state.addConn(c, s.maxConnsPerDoc()) {
		return nil, state, false, nil
	}
	s.totalConns.Add(1)
	return c, state, true, nil
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

	// Drop the global connection count. releaseConn runs exactly once per
	// admitted connection (serveWS defers it only past a successful
	// admission, and the white-box callers release only admitConn results
	// with ok==true), so this pairs one-to-one with the increment in
	// admitConn and the count never drifts.
	s.totalConns.Add(-1)

	tombstoned := c.handler.Disconnect()
	if len(tombstoned) > 0 {
		// Broadcast the resulting awareness removals to remaining
		// peers so they learn this peer's clients departed.
		state.broadcastAwarenessRemovals(tombstoned)
	}

	if remaining > 0 {
		return
	}

	// Evict atomically: under docsMu, confirm the registry still maps this
	// exact docState (identity, not just name) and it still has no
	// connections (rechecked under connsMu). In the window since we read
	// remaining==0, a concurrent admitConn (which holds docsMu across its
	// own insert) could have admitted a new connection onto this state —
	// deleting then would orphan a live document and split its clients
	// onto divergent Docs that never converge. The connsMu recheck is what
	// observes that insert and skips the delete. Lock order docsMu ->
	// connsMu is not nested anywhere else.
	evicted := false
	s.docsMu.Lock()
	if cur, ok := s.docs[state.name]; ok && cur == state {
		state.connsMu.Lock()
		if len(state.conns) == 0 {
			delete(s.docs, state.name)
			evicted = true
		}
		state.connsMu.Unlock()
	}
	s.docsMu.Unlock()

	if evicted {
		// Stop the backplane subscription for the now-departed document, so
		// its handler no longer applies foreign updates to a detached copy.
		// Done outside docsMu (like the Flush below) to keep any broker I/O
		// off the registry lock; the brief window in which a foreign update
		// could still reach the detached doc is harmless (it has no
		// connections, and a re-admit resubscribes on a fresh copy).
		if state.backplaneUnsub != nil {
			state.backplaneUnsub()
		}
		if s.opts.Store != nil {
			// Flush is best-effort; the document log is intact in the Store
			// either way. Only the evicting releaser flushes, so a
			// re-acquired live doc is never flushed out from under its
			// connections.
			_ = s.opts.Store.Flush(ctx, state.name)
		}
	}
}

// addConn registers a connection in the docState's set, enforcing the
// per-document cap atomically under connsMu. It returns false without
// adding when the room is already at limit; a negative limit means
// unlimited. The connection's broadcast callback fan-outs to this set.
func (s *docState) addConn(c *conn, limit int) bool {
	s.connsMu.Lock()
	defer s.connsMu.Unlock()
	if limit >= 0 && len(s.conns) >= limit {
		return false
	}
	s.conns[c] = struct{}{}
	return true
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

// send writes one envelope to the underlying WS under a bounded
// deadline. The writeMu serializes concurrent writes (a broadcast on a
// peer's goroutine can race with a Send on this conn's read goroutine).
//
// A slow or half-open peer would otherwise block the write forever and
// back-pressure the whole fan-out (and the global sweep). On timeout we
// close the peer so its read loop unblocks and releaseConn evicts it,
// and subsequent broadcasts skip it.
func (c *conn) send(envelope []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), c.writeTimeout())
	defer cancel()
	if err := c.ws.Write(ctx, websocket.MessageBinary, envelope); err != nil {
		_ = c.ws.Close(websocket.StatusPolicyViolation, "write timeout")
		return err
	}
	return nil
}

// writeTimeout resolves the per-write deadline from the server options,
// falling back to defaultWriteTimeout.
func (c *conn) writeTimeout() time.Duration {
	if c.server != nil && c.server.opts.WriteTimeout > 0 {
		return c.server.opts.WriteTimeout
	}
	return defaultWriteTimeout
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
	// Persist and notify on applied document updates. We dispatch here
	// rather than inside the sync handler so the persistence and
	// side-effect concerns stay in the transport layer.
	if c.server == nil {
		return
	}
	c.onAppliedUpdate(envelope)
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

// applyBackplaneUpdate applies a document update received from another
// instance over the Backplane to this server's in-memory copy and
// re-broadcasts it to the document's local connections. It deliberately
// does NOT persist (the originating instance already did, to the shared
// Store), does NOT fire OnChange (that fires once, on the instance whose
// client made the edit), and does NOT re-publish (which would echo back
// into the cluster). Runs on a backplane-owned goroutine; state.doc's own
// locking makes the apply safe against concurrent client edits, and the
// re-broadcast reuses the ordinary per-conn write path.
func (s *Server) applyBackplaneUpdate(state *docState, update []byte) {
	if err := encoding.ApplyUpdate(state.doc, update); err != nil {
		log.Printf("server: backplane apply for %q: %v", state.name, err)
		return
	}
	envelope := syncpkg.EncodeSyncUpdate(update)
	for _, peer := range state.snapshotConns() {
		_ = peer.send(envelope)
	}
}

// onAppliedUpdate handles a broadcast envelope that carries a real
// document change: it persists the update to the Store (if configured)
// and invokes OnChange (if configured). Awareness frames and SyncStep1
// carry no document mutation and are ignored; SyncStep2 IS handled
// because it delivers content the server did not have before. A
// read-only connection's update never reaches here — the handler drops
// it before broadcast.
func (c *conn) onAppliedUpdate(envelope []byte) {
	if c.server.opts.Store == nil && c.server.opts.OnChange == nil && c.server.opts.Backplane == nil {
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
	if c.server.opts.Store != nil {
		if err := c.server.opts.Store.StoreUpdate(context.Background(), c.state.name, frame.Payload); err != nil {
			// A failed persist must not be invisible, and must not mark
			// the document dirty: auto-versioning would then capture
			// in-memory state that was never durably stored. Log and skip
			// the dirty mark, but still notify OnChange below — the
			// document did change in memory and on peers.
			log.Printf("server: persist update for %q: %v", c.state.name, err)
		} else {
			c.server.markVersionDirty(c.state.name)
		}
	}
	if c.server.opts.OnChange != nil {
		c.server.opts.OnChange(c.state.name, frame.Payload)
	}
	if c.server.opts.Backplane != nil {
		// Fan this locally-applied update out to the other instances. A
		// failure is logged, not fatal to THIS instance: the update is
		// already applied, broadcast to local clients, and (if configured)
		// persisted. But a dropped publish is a dropped causal dependency on
		// a peer, not a single lost delta: a peer that misses it silently
		// parks every later edit from that client until the document reloads
		// from the shared Store (which only happens on eviction), so a
		// backplane with at-most-once delivery trades this away by design.
		if err := c.server.opts.Backplane.Publish(context.Background(), c.state.name, frame.Payload); err != nil {
			log.Printf("server: backplane publish for %q: %v", c.state.name, err)
		}
	}
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
