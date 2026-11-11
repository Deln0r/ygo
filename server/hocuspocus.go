package server

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	syncpkg "github.com/Deln0r/ygo/internal/sync"
)

const (
	hocuspocusTimeout  = 60 * time.Second
	maxPendingKeys     = 100
	maxPendingMessages = 1000
	maxPendingBytes    = 5 << 20
)

// HocuspocusHandler serves document-prefixed frames at any URL path. Mount
// it on the exact endpoint providers use (e.g. "/collab"): providers strip
// trailing slashes. Handler also serves these sessions at an empty doc name.
func (s *Server) HocuspocusHandler() http.Handler {
	return http.HandlerFunc(s.serveHocuspocus)
}

// SendStateless broadcasts a message to the document's Hocuspocus keys.
// It queues without waiting for clients; it does not load an absent room.
func (s *Server) SendStateless(docName, payload string) {
	s.docsMu.Lock()
	state := s.docs[docName]
	s.docsMu.Unlock()
	if state != nil {
		state.broadcastStateless(syncpkg.EncodeStateless(payload))
	}
}

func (s *docState) broadcastStateless(envelope []byte) {
	for _, peer := range s.snapshotConns() {
		if peer.session != nil {
			peer.enqueue(envelope)
		}
	}
}

func (c *conn) routed(envelope []byte) []byte {
	if len(c.route) == 0 {
		return envelope
	}
	out := make([]byte, 0, len(c.route)+len(envelope))
	out = append(out, c.route...)
	return append(out, envelope...)
}

// hocuspocusSession owns one socket, writer and send queue. Its read loop
// alone owns keys and pending; document broadcasts only use conn.enqueue.
// The transport is a conn without a document, reusing the bounded writer
// and close path used by y-websocket.
type hocuspocusSession struct {
	server                        *Server
	transport                     *conn
	request                       *http.Request
	keys                          map[string]*conn
	pending                       map[string][][]byte
	pendingBytes, pendingMessages int
	onAdmission                   func()
}

func (s *Server) serveHocuspocus(w http.ResponseWriter, r *http.Request) {
	aw, rec := recordHijack(w)
	ws, err := websocket.Accept(aw, r, &websocket.AcceptOptions{OriginPatterns: s.opts.OriginPatterns})
	if err != nil {
		return
	}
	t := &conn{server: s, ws: ws, queue: newSendQueue(s.maxSendQueue(), s.maxSendQueueBytes(), s.writeTimeout())}
	if rec != nil {
		t.raw = rec.conn
	}
	limit := s.opts.ReadLimit
	if limit == 0 {
		limit = defaultReadLimit
	}
	ws.SetReadLimit(limit)

	// Reserve a socket slot before accepting any keys, including sockets
	// which never authenticate. Key admission does not consume another slot.
	s.docsMu.Lock()
	full := s.maxConns() > 0 && s.totalConns.Load() >= int64(s.maxConns())
	if !full {
		s.totalConns.Add(1)
	}
	s.docsMu.Unlock()
	if full {
		t.closeWS(websocket.StatusTryAgainLater, "server connection limit reached")
		return
	}
	defer s.totalConns.Add(-1)
	t.startWriter()
	defer func() {
		t.queue.terminate(causeReleased)
		t.finishTransport()
	}()
	session := &hocuspocusSession{
		server: s, transport: t, request: r,
		keys: make(map[string]*conn), pending: make(map[string][][]byte),
	}
	defer session.releaseAll()

	idle := s.opts.IdleTimeout
	if idle <= 0 {
		idle = hocuspocusTimeout
	}
	auth := s.opts.AuthTimeout
	if auth <= 0 {
		auth = hocuspocusTimeout
	}
	// Timer callbacks never touch the key maps. Stop and join them before
	// releasing the socket, including a callback already inside Close.
	var timers sync.WaitGroup
	startTimer := func(d time.Duration, reason string) *time.Timer {
		timers.Add(1)
		return time.AfterFunc(d, func() {
			defer timers.Done()
			t.closeFromServer(4408, reason)
		})
	}
	idleTimer := startTimer(idle, "connection timeout")
	authTimer := startTimer(auth, "authentication timeout")
	session.onAdmission = func() {
		if authTimer != nil {
			if authTimer.Stop() {
				timers.Done()
			}
			authTimer = nil
		}
	}
	// Resetting an AfterFunc whose callback has begun could run it twice.
	// Use Stop before Reset, and end the read loop when Stop loses the race.
	defer func() {
		if idleTimer.Stop() {
			timers.Done()
		}
		session.onAdmission()
		timers.Wait()
	}()
	for {
		kind, raw, err := ws.Read(r.Context())
		if err != nil {
			return
		}
		if !idleTimer.Stop() {
			return
		}
		idleTimer.Reset(idle)
		if kind != websocket.MessageBinary {
			t.closeFromServer(4401, "expected a binary Hocuspocus frame")
			return
		}
		if !session.handle(raw) {
			return
		}
	}
}

func (s *hocuspocusSession) reply(key string, envelope []byte) error {
	return s.transport.reply(append(syncpkg.EncodeRoute(key), envelope...))
}

func (s *hocuspocusSession) dropPending(key string) [][]byte {
	frames := s.pending[key]
	delete(s.pending, key)
	for _, raw := range frames {
		s.pendingBytes -= len(raw)
		s.pendingMessages--
	}
	return frames
}

func (s *hocuspocusSession) handle(raw []byte) bool {
	key, envelope, err := syncpkg.DecodeRoute(raw)
	if err != nil {
		s.transport.closeFromServer(4401, "invalid document routing key")
		return false
	}
	if syncpkg.RouteDocName(key) == "" {
		s.transport.closeFromServer(websocket.StatusPolicyViolation, "no document name in the URL")
		return false
	}
	frame, _, err := syncpkg.DecodeEnvelope(envelope)
	if err != nil {
		if errors.Is(err, syncpkg.ErrUnknownSyncSubType) && s.keys[key] != nil {
			s.releaseKey(key)
			return s.reply(key, syncpkg.EncodeClose("Reset Connection")) == nil
		}
		s.transport.closeFromServer(4401, "invalid Hocuspocus envelope")
		return false
	}
	if frame.Type == syncpkg.MessageClose {
		s.dropPending(key)
		if s.keys[key] != nil {
			s.releaseKey(key)
			return s.reply(key, syncpkg.EncodeClose("provider_initiated")) == nil
		}
		return true
	}
	isToken := frame.Type == syncpkg.MessageAuth && frame.AuthSub == syncpkg.AuthToken
	if frame.Type == syncpkg.MessageAuth && !isToken {
		return true
	}
	c := s.keys[key]
	if c == nil {
		opts := s.server.opts
		needsToken := opts.OnAuthenticateRequest != nil || opts.OnAuthenticate != nil
		if needsToken && !isToken {
			if _, exists := s.pending[key]; !exists && len(s.pending) >= maxPendingKeys {
				s.transport.closeFromServer(4205, "too many pending documents")
				return false
			}
			if s.pendingMessages >= maxPendingMessages || len(raw) > maxPendingBytes-s.pendingBytes {
				s.transport.closeFromServer(4205, "unauthenticated message queue limit reached")
				return false
			}
			s.pending[key] = append(s.pending[key], raw)
			s.pendingMessages++
			s.pendingBytes += len(raw)
			return true
		}
		readOnly := false
		if isToken {
			if opts.OnAuthenticateRequest != nil {
				readOnly, err = opts.OnAuthenticateRequest(s.request, syncpkg.RouteDocName(key), string(frame.Payload))
			} else if opts.OnAuthenticate != nil {
				err = opts.OnAuthenticate(syncpkg.RouteDocName(key), string(frame.Payload))
			}
			if err != nil {
				s.dropPending(key)
				reason := err.Error()
				if reason == "" {
					reason = "permission-denied"
				}
				return s.reply(key, syncpkg.EncodeAuthPermissionDenied(reason)) == nil
			}
		}
		if s.transport.queue.terminalCause() != causeNone {
			return false
		}
		var ok bool
		c, _, ok, err = s.server.admitDocument(s.request.Context(), syncpkg.RouteDocName(key), s.transport.ws, s, key)
		if err != nil || !ok {
			s.transport.closeFromServer(websocket.StatusTryAgainLater, "document admission failed")
			return false
		}
		// Register before adopter hooks, so a panic still releases the room.
		s.keys[key] = c
		c.accepted = opts.OnConnect == nil
		if opts.OnConnect != nil {
			if err := opts.OnConnect(c.idForLog, c.state.name, s.request); err != nil {
				s.releaseKey(key)
				s.dropPending(key)
				return s.reply(key, syncpkg.EncodeAuthPermissionDenied(err.Error())) == nil
			}
			c.accepted = true
		}
		if opts.ReadOnly != nil && opts.ReadOnly(c.state.name, s.request) {
			readOnly = true
		}
		c.handler.ReadOnly = readOnly
		// Admission ends the absolute auth deadline now, before a slow
		// reply or draining queued messages. IdleTimeout still applies.
		s.onAdmission()
		if err := c.activateHocuspocus(isToken); err != nil {
			return false
		}
		if snap := c.state.awareness.EncodeLive(); len(snap) > 0 && snap[0] != 0 {
			if err := c.reply(syncpkg.EncodeAwareness(snap)); err != nil {
				return false
			}
		}
		queued := s.dropPending(key)
		for _, prior := range queued {
			_, body, _ := syncpkg.DecodeRoute(prior)
			f, _, _ := syncpkg.DecodeEnvelope(body) // validated when queued
			if !s.dispatch(key, c, f) {
				return false
			}
			if s.keys[key] == nil {
				return true
			}
		}
		if isToken {
			return true
		}
	} else if isToken {
		// Token refresh is not implemented: the established key retains its
		// scope. Each Token still gets the response providers wait for.
		return s.replyAuth(key, c) == nil
	}
	return s.dispatch(key, c, frame)
}

// activateHocuspocus places the acknowledgement ahead of broadcasts while
// holding sendMu only for the queue append. A slow auth reply must not hold
// up the room, and a broadcast after the peer sees it must not be dropped.
func (c *conn) activateHocuspocus(auth bool) error {
	c.sendMu.Lock()
	if !auth {
		c.ready = true
		c.sendMu.Unlock()
		return nil
	}
	scope := syncpkg.ScopeReadWrite
	if c.handler.ReadOnly {
		scope = syncpkg.ScopeReadOnly
	}
	result, err := c.queue.pushReply(c.routed(syncpkg.EncodeAuthAuthenticated(scope)))
	c.ready = err == nil
	c.sendMu.Unlock()
	if err != nil {
		return err
	}
	return <-result
}

func (s *hocuspocusSession) replyAuth(key string, c *conn) error {
	scope := syncpkg.ScopeReadWrite
	if c.handler.ReadOnly {
		scope = syncpkg.ScopeReadOnly
	}
	return s.reply(key, syncpkg.EncodeAuthAuthenticated(scope))
}

func (s *hocuspocusSession) dispatch(key string, c *conn, f *syncpkg.Frame) bool {
	if err := c.handler.HandleFrame(f); err != nil {
		if s.transport.queue.terminalCause() != causeNone {
			return false
		}
		s.releaseKey(key)
		return s.reply(key, syncpkg.EncodeClose("Reset Connection")) == nil
	}
	return true
}

func (s *hocuspocusSession) releaseKey(key string) {
	c := s.keys[key]
	delete(s.keys, key)
	c.sendMu.Lock()
	c.ready = false
	c.sendMu.Unlock()
	// Cleanup must still persist after the request or read context ends.
	s.server.releaseConn(context.Background(), c.state, c)
	if c.accepted && s.server.opts.OnDisconnect != nil {
		s.server.opts.OnDisconnect(c.idForLog, c.state.name)
	}
}

func (s *hocuspocusSession) releaseAll() {
	// Defer each release so an adopter's panicking OnDisconnect cannot
	// strand the other keys. Socket cleanup is independently deferred.
	for key := range s.keys {
		defer s.releaseKey(key)
	}
}

// looksLikeHocuspocus recognizes a document-prefixed first frame on the
// y-websocket endpoint. Sync begins with zero, never a non-empty key.
func looksLikeHocuspocus(raw []byte) bool {
	key, body, err := syncpkg.DecodeRoute(raw)
	if err != nil || key == "" || len(body) == 0 {
		return false
	}
	f, _, err := syncpkg.DecodeEnvelope(body)
	return err == nil && (f.Type == syncpkg.MessageAuth || f.Type == syncpkg.MessageSync || f.Type == syncpkg.MessageAwareness)
}
