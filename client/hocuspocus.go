package client

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/coder/websocket"

	syncpkg "github.com/Deln0r/ygo/internal/sync"
)

// Errors a Hocuspocus server can cause, reported through OnError wrapped
// with the server's reason.
var (
	// ErrPermissionDenied: the server's onAuthenticate refused the token.
	// The client closes the connection and dials again after at least
	// deniedBackoff, asking TokenFunc for a token again. To stop, call
	// Close from another goroutine (go c.Close()): Close waits for the
	// goroutine that runs OnError.
	ErrPermissionDenied = errors.New("client: permission denied")

	// ErrDocumentClosed: the server closed the document on this
	// connection (a hook's error, its idle timeout, a reset). The client
	// dials again.
	ErrDocumentClosed = errors.New("client: the server closed the document")

	// ErrUpdateRefused: the server did not apply an update this client
	// sent (SyncStatus 0: a read-only connection). The session goes on.
	ErrUpdateRefused = errors.New("client: the server did not apply an update")
)

// deniedBackoff is the least a dial waits after a permission denial or a
// TokenFunc error, so a refused or missing token does not cost the server
// an upgrade and an onAuthenticate run at the reconnect rate.
var deniedBackoff = 30 * time.Second

// errTokenFunc wraps a TokenFunc error at connect.
var errTokenFunc = errors.New("client: TokenFunc")

// afterSendClose, set by tests, runs between Close's CLOSE and the
// cancel, to widen the window in which the server's answer arrives.
var afterSendClose func()

// hpSession is one connection's Hocuspocus state.
type hpSession struct {
	answered     chan struct{} // closed once the server's SyncStep1 is answered
	answeredOnce sync.Once
	mu           sync.Mutex
	err          error // what ended the session, from the server's frames
}

func (s *hpSession) answer() { s.answeredOnce.Do(func() { close(s.answered) }) }

func (s *hpSession) fail(err error) {
	s.mu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.mu.Unlock()
}

func (s *hpSession) failure() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Scope is the scope the Hocuspocus server granted this connection
// ("read-write" or "readonly"), or "" before it authenticated and between
// connections.
func (c *Client) Scope() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.scope
}

// Unsynced is the number of this client's sync messages a Hocuspocus
// server has not yet confirmed applying, as the provider's
// unsyncedChanges counts them: 1 at every connect (the reply to the
// server's SyncStep1), one more per update sent, one less per SyncStatus
// that says applied. 0 means the server has everything this client sent.
// A refused update stays counted. Always 0 outside Hocuspocus mode.
func (c *Client) Unsynced() int {
	if !c.opts.Hocuspocus {
		return 0
	}
	return int(c.unsynced.Load())
}

// SendStateless sends a Hocuspocus stateless message (type 5) to the
// server's onStateless hook.
func (c *Client) SendStateless(payload string) error {
	if !c.opts.Hocuspocus {
		return errors.New("client: SendStateless needs Options.Hocuspocus")
	}
	return c.write(syncpkg.EncodeStateless(payload))
}

// token returns the token for an Auth frame.
func (c *Client) token(ctx context.Context) (string, error) {
	if c.opts.TokenFunc != nil {
		return c.opts.TokenFunc(ctx)
	}
	return c.opts.Token, nil
}

// hocusHandshake opens a Hocuspocus session: Auth with the token, then
// SyncStep1, then the awareness state if one is set, as providers send
// them. No QueryAwareness: no server answers it.
func (c *Client) hocusHandshake(tok string) error {
	c.unsynced.Store(1)
	if err := c.write(syncpkg.EncodeAuthToken(tok)); err != nil {
		return err
	}
	if err := c.sendStep1(); err != nil {
		return err
	}
	if c.awareness.RenewLocalState(0) {
		c.sendAwareness()
	}
	return nil
}

// handleHocus handles the frame types only Hocuspocus servers send. It
// reports whether the frame was one of them.
func (c *Client) handleHocus(s *hpSession, frame *syncpkg.Frame) bool {
	switch frame.Type {
	case syncpkg.MessageAuth:
		switch frame.AuthSub {
		case syncpkg.AuthAuthenticated:
			scope := string(frame.Payload)
			c.mu.Lock()
			c.scope = scope
			c.mu.Unlock()
			if c.opts.OnAuthenticated != nil {
				c.opts.OnAuthenticated(scope)
			}
		case syncpkg.AuthPermissionDenied:
			s.fail(fmt.Errorf("%w: %s", ErrPermissionDenied, frame.Payload))
			c.dropConn()
		case syncpkg.AuthToken:
			// The server asks for the token again (servers 3 and 4, when
			// the app or an extension calls requestToken). The refresh runs
			// off the read loop, so the loop still sees the connection
			// close, which ends the refresh's context; the token goes to
			// the connection that asked, and not at all once its session
			// is over (a later session's server would take a stale token
			// as a refresh, and its onTokenSync could close it). A failed
			// write is the session's read error's to report.
			c.mu.Lock()
			conn, ctx := c.conn, c.connCtx
			c.mu.Unlock()
			if conn != nil && ctx != nil {
				go func() {
					tok, err := c.token(ctx)
					if ctx.Err() != nil {
						return
					}
					if err != nil {
						if c.opts.OnError != nil {
							c.opts.OnError(fmt.Errorf("client: token refresh: %w", err))
						}
						return
					}
					_ = conn.Write(ctx, websocket.MessageBinary, append(syncpkg.EncodeRoute(c.opts.DocName), syncpkg.EncodeAuthToken(tok)...))
				}()
			}
		}
		return true
	case syncpkg.MessageSyncStatus:
		if len(frame.Payload) > 0 && frame.Payload[0] == 1 {
			for {
				n := c.unsynced.Load()
				if n <= 0 || c.unsynced.CompareAndSwap(n, n-1) {
					break
				}
			}
		} else if c.opts.OnError != nil {
			c.opts.OnError(ErrUpdateRefused)
		}
		return true
	case syncpkg.MessageClose:
		c.mu.Lock()
		closing := c.closed
		c.mu.Unlock()
		if !closing {
			s.fail(fmt.Errorf("%w: %s", ErrDocumentClosed, frame.Payload))
		}
		c.dropConn()
		return true
	case syncpkg.MessageStateless:
		if c.opts.OnStateless != nil {
			c.opts.OnStateless(string(frame.Payload))
		}
		return true
	case syncpkg.MessageBroadcastStateless, syncpkg.MessagePing, syncpkg.MessagePong:
		return true // no server sends these to a provider; never answered
	}
	return false
}

// dropConn closes the current connection at once; the read loop then
// ends the session.
func (c *Client) dropConn() {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn != nil {
		_ = conn.CloseNow()
	}
}

// sendClose tells a Hocuspocus server this client is done with the
// document, before Close cancels the session. It uses a context of its
// own: the session's is about to be cancelled.
func (c *Client) sendClose() {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	// A provider's CLOSE is the type alone, with no reason.
	_ = conn.Write(ctx, websocket.MessageBinary, append(syncpkg.EncodeRoute(c.opts.DocName), byte(syncpkg.MessageClose)))
}
