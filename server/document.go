package server

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/Deln0r/ygo/internal/doc"
	"github.com/Deln0r/ygo/internal/encoding"
	syncpkg "github.com/Deln0r/ygo/internal/sync"
)

// Changing documents from the server: OpenDocument, Apply, ApplyUpdate and
// CloseDocument. A server write goes the way a client's update goes: it is
// applied to the server's copy of the document, queued to every connection
// of the document (y-websocket and Hocuspocus, read-only ones included),
// stored in the Store, published on the Backplane and reported to OnChange.

var (
	// ErrDocumentNotFound is returned by CloseDocument for a document the
	// server does not hold.
	ErrDocumentNotFound = errors.New("server: document not resident")
	// ErrDocumentHasConnections is returned by CloseDocument without force
	// for a document that still has connections.
	ErrDocumentHasConnections = errors.New("server: document has connections")
	// ErrWriteRefused wraps the error Options.OnServerWrite refused a
	// server write with.
	ErrWriteRefused = errors.New("server: write refused by OnServerWrite")
	// ErrNotStored wraps the Store's error for a server write that was
	// applied and sent but not stored.
	ErrNotStored = errors.New("server: write applied and sent but not stored")
	// ErrTooManyDocuments is returned when loading the document would pass
	// Options.MaxDocs.
	ErrTooManyDocuments = errServerDocsFull
	// ErrDocumentClosed is returned by a Document after its Close.
	ErrDocumentClosed = errors.New("server: Document is closed")
	// ErrInvalidUpdate wraps the decoding error of an update ApplyUpdate
	// was given, or reports bytes after one complete update.
	ErrInvalidUpdate = errors.New("server: invalid V1 update")
)

// ServerOrigin is the origin of the transaction a server write commits:
// each write has its own value, so Doc.OnUpdate handlers and observers on a
// server's document can tell server writes (and which one) from clients'
// updates with a type assertion.
type ServerOrigin struct {
	DocName string
}

// ServerWrite describes a server write to Options.OnServerWrite.
type ServerWrite struct {
	DocName string
	// Op is "apply" (Apply) or "applyUpdate" (ApplyUpdate).
	Op string
	// Size is the update's length for ApplyUpdate, 0 for Apply (the update
	// does not exist yet).
	Size int
}

// Document is a server document held open by OpenDocument: it stays
// resident until Close, whether or not clients are connected. Its methods
// are safe for concurrent use.
type Document struct {
	s      *Server
	st     *docState
	mu     sync.Mutex
	closed bool
	// active counts calls in progress: Close waits for them, so a write
	// never lands on a document already let go.
	active sync.WaitGroup
}

// OpenDocument loads the document as a connection would (from the Store,
// then OnLoadDocument; counted against MaxDocs) or finds it resident, and
// holds it open until the Document's Close. While a CloseDocument closes the
// document, it waits for that to finish and loads it afresh. After Close it
// returns ErrServerClosed.
//
// OnLoadDocument and the Store's load run under the server's document
// registry lock, as for a connection: they must not call the server's
// document API.
func (s *Server) OpenDocument(ctx context.Context, docName string) (*Document, error) {
	if docName == "" {
		return nil, errors.New("server: empty document name")
	}
	st, err := s.pin(ctx, docName)
	if err != nil {
		return nil, err
	}
	return &Document{s: s, st: st}, nil
}

// pin makes docName resident and counts a hold on it (docState.pins), and
// an operation Close waits for.
func (s *Server) pin(ctx context.Context, docName string) (*docState, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := s.waitNotClosing(ctx, docName, true, nil); err != nil {
			return nil, err
		}
		st, err := s.pinOnce(ctx, docName)
		if errors.Is(err, errDocClosing) {
			continue
		}
		return st, err
	}
}

// pinOnce is one attempt of pin under docsMu, released by defer: the load
// (Store, OnLoadDocument) runs under it and may panic.
func (s *Server) pinOnce(ctx context.Context, docName string) (*docState, error) {
	s.docsMu.Lock()
	defer s.docsMu.Unlock()
	if s.closed {
		return nil, ErrServerClosed
	}
	st, err := s.getOrCreateDocLocked(ctx, docName)
	if err != nil {
		return nil, err
	}
	st.pins++
	st.idleSince = time.Time{}
	s.ops.Add(1)
	return st, nil
}

// unpin drops a hold pin took; the last one settles a document without
// connections as its last disconnect would.
func (s *Server) unpin(st *docState) {
	s.docsMu.Lock()
	st.pins--
	s.docsMu.Unlock()
	s.settle(context.Background(), st)
	s.ops.Done()
}

// Name is the document's name.
func (d *Document) Name() string { return d.st.name }

// Close lets the document go: with no connection and no other open
// Document left it is parked (DocIdleTimeout) or evicted with a final
// flush, as after its last disconnect. On a server without a Store, a
// document a server write changed stays resident instead (until
// CloseDocument or the server's Close), so the write is not lost. Close is
// idempotent.
func (d *Document) Close() {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return
	}
	d.closed = true
	d.mu.Unlock()
	d.active.Wait()
	d.s.unpin(d.st)
}

// begin admits a call: refused after the Document's Close
// (ErrDocumentClosed) or the server's (ErrServerClosed); end ends it.
func (d *Document) begin() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return ErrDocumentClosed
	}
	d.s.docsMu.Lock()
	closed := d.s.closed
	d.s.docsMu.Unlock()
	if closed {
		return ErrServerClosed
	}
	d.active.Add(1)
	return nil
}

func (d *Document) end() { d.active.Done() }

// State is the server's copy of the document as one V1 update
// (EncodeStateAsUpdate), the pending buffer included.
func (d *Document) State() ([]byte, error) {
	if err := d.begin(); err != nil {
		return nil, err
	}
	defer d.end()
	return encoding.EncodeStateAsUpdateWithPending(d.st.doc), nil
}

// Apply runs fn in one write transaction on the server's copy of the
// document and sends what it changed as a client's update is sent: queued
// to every connection, stored, published, reported to OnChange. It returns
// that update, or nil when fn changed nothing (nothing is then sent).
//
// The transaction's origin is a new *ServerOrigin. fn runs under the
// document's write lock: take roots with the *In accessors (ygo.TextIn(txn,
// "t")), and do not call the server for the same document or wait on its
// clients. Whatever fn changed is committed and sent even when it returns
// an error or panics (CRDT edits are not rolled back, and the clients must
// see what the server holds); the error is returned after that, a panic
// goes on after that. Updates of follow-up transactions the server's own
// observers open during the commit (TextEvent.ReadDelta) are not sent, as
// for a client's update.
//
// A Store error is returned wrapped in ErrNotStored, after the update was
// applied and sent.
func (d *Document) Apply(ctx context.Context, fn func(txn *doc.TransactionMut) error) ([]byte, error) {
	if err := d.begin(); err != nil {
		return nil, err
	}
	defer d.end()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s, st := d.s, d.st
	if h := s.opts.OnServerWrite; h != nil {
		if err := h(ctx, ServerWrite{DocName: st.name, Op: "apply"}); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrWriteRefused, err)
		}
	}
	origin := &ServerOrigin{DocName: st.name}
	// The handler runs inside the commit, under the document's lock, on
	// this goroutine for this transaction: only its update has origin.
	var captured []byte
	unsub := st.doc.OnUpdate(func(u []byte, o any) {
		if o == origin {
			captured = append([]byte(nil), u...)
		}
	})
	var fnErr error
	var fnPanic any
	panicked := false
	func() {
		defer unsub()
		txn := st.doc.WriteTxn()
		txn.Origin = origin
		defer txn.Commit()
		defer func() {
			if r := recover(); r != nil {
				fnPanic, panicked = r, true
			}
		}()
		fnErr = fn(txn)
	}()
	var storeErr error
	if captured != nil {
		storeErr = s.sendServerUpdate(st, captured)
	}
	if panicked {
		panic(fnPanic)
	}
	if storeErr != nil {
		storeErr = fmt.Errorf("%w: %w", ErrNotStored, storeErr)
	}
	return captured, errors.Join(fnErr, storeErr)
}

// ApplyUpdate applies a V1 update to the server's copy of the document and
// sends it, as given, as a client's update is sent: queued to every
// connection, stored, published, reported to OnChange. The update is
// checked first: it must decode, and nothing may follow it
// (ErrInvalidUpdate); nothing is applied otherwise. Unlike a fan-out that
// does not apply, the server's state then holds the update, so a client that
// connects later gets it in its sync.
//
// A Store error is returned wrapped in ErrNotStored, after the update was
// applied and sent.
func (d *Document) ApplyUpdate(ctx context.Context, update []byte) error {
	if err := d.begin(); err != nil {
		return err
	}
	defer d.end()
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, rest, err := encoding.DecodeUpdate(update); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidUpdate, err)
	} else if len(rest) > 0 {
		return fmt.Errorf("%w: %d bytes after one complete update", ErrInvalidUpdate, len(rest))
	}
	s, st := d.s, d.st
	if h := s.opts.OnServerWrite; h != nil {
		if err := h(ctx, ServerWrite{DocName: st.name, Op: "applyUpdate", Size: len(update)}); err != nil {
			return fmt.Errorf("%w: %w", ErrWriteRefused, err)
		}
	}
	if err := encoding.ApplyUpdateWithOrigin(st.doc, update, &ServerOrigin{DocName: st.name}); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidUpdate, err)
	}
	if err := s.sendServerUpdate(st, append([]byte(nil), update...)); err != nil {
		return fmt.Errorf("%w: %w", ErrNotStored, err)
	}
	return nil
}

// sendServerUpdate sends an update a server write applied: retained on a
// server without a Store (see Document.Close), queued to the document's
// connections, then stored, published and reported (afterUpdate).
func (s *Server) sendServerUpdate(st *docState, update []byte) error {
	if s.opts.Store == nil {
		s.docsMu.Lock()
		st.retain = true
		s.docsMu.Unlock()
	}
	envelope := syncpkg.EncodeSyncUpdate(update)
	for _, peer := range st.snapshotConns() {
		peer.enqueue(envelope)
	}
	return s.afterUpdate(st, update)
}

// Apply opens docName, runs Document.Apply and closes it.
func (s *Server) Apply(ctx context.Context, docName string, fn func(txn *doc.TransactionMut) error) ([]byte, error) {
	d, err := s.OpenDocument(ctx, docName)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	return d.Apply(ctx, fn)
}

// ApplyUpdate opens docName, runs Document.ApplyUpdate and closes it.
func (s *Server) ApplyUpdate(ctx context.Context, docName string, update []byte) error {
	d, err := s.OpenDocument(ctx, docName)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.ApplyUpdate(ctx, update)
}

// CloseDocument takes a resident document out of the server. Without force
// it refuses a document with connections (ErrDocumentHasConnections). With
// force, its y-websocket connections and the Hocuspocus sockets holding it
// are closed with 1001; such a socket's other documents reconnect, as
// providers do after any drop (docs/tech-debt.md says why one document of a
// socket is not closed alone). Do not call it from a hook for a document the
// hook's own connection holds: it would wait until ctx ends. It then waits
// for those connections and any open
// Document to go, evicts the document as after its last disconnect
// (Backplane unsubscribe, final flush; a retained document is dropped) and
// returns. Connections and OpenDocument calls for the name wait meanwhile
// and then load it afresh. A cancelled ctx returns early; the close then
// finishes on its own.
func (s *Server) CloseDocument(ctx context.Context, docName string, force bool) error {
	s.docsMu.Lock()
	if s.closed {
		s.docsMu.Unlock()
		return ErrServerClosed
	}
	st := s.docs[docName]
	if st == nil {
		s.docsMu.Unlock()
		return ErrDocumentNotFound
	}
	if ch := st.closing; ch != nil {
		s.docsMu.Unlock()
		select {
		case <-ch:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	st.connsMu.RLock()
	n := len(st.conns)
	st.connsMu.RUnlock()
	if n > 0 && !force {
		s.docsMu.Unlock()
		return ErrDocumentHasConnections
	}
	st.closing = make(chan struct{})
	st.closeWake = make(chan struct{}, 1)
	// Not parked while it closes: the idle sweep and MaxIdleDocs only take
	// parked documents.
	st.idleSince = time.Time{}
	s.ops.Add(1)
	s.docsMu.Unlock()
	done := make(chan struct{})
	go func() {
		defer s.ops.Done()
		defer close(done)
		s.closeDocument(st)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// closeDocument is CloseDocument's work once st is marked closing.
func (s *Server) closeDocument(st *docState) {
	// A connection releases itself on its own goroutine once its read loop
	// ends, after any frame it is handling: closing its socket is what lets
	// it go. A Hocuspocus session is closed as a whole (its other documents
	// reconnect, as providers do after any drop): its read loop alone owns
	// its keys and their frames in progress.
	for _, c := range st.snapshotConns() {
		if c.session != nil {
			c.session.transport.closeFromServer(websocket.StatusGoingAway, closedByServerReason)
		} else {
			c.closeFromServer(websocket.StatusGoingAway, closedByServerReason)
		}
	}
	for {
		s.docsMu.Lock()
		st.connsMu.RLock()
		n := len(st.conns)
		st.connsMu.RUnlock()
		if n == 0 && st.pins == 0 {
			s.docsMu.Unlock()
			break
		}
		wake := st.closeWake
		s.docsMu.Unlock()
		// Woken by releaseConn or an unpin (settle); the timer only bounds
		// a missed wake.
		select {
		case <-wake:
		case <-time.After(100 * time.Millisecond):
		}
	}
	// The document stays registered (closing) until its eviction is done,
	// so nothing loads a fresh copy before the final flush.
	s.finishEviction(context.Background(), st)
	s.docsMu.Lock()
	if s.docs[st.name] == st {
		delete(s.docs, st.name)
	}
	ch := st.closing
	st.closing = nil
	s.docsMu.Unlock()
	close(ch)
}
