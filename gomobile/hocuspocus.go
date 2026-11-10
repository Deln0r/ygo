package gomobile

import (
	"context"
	"errors"
	"strings"

	"github.com/Deln0r/ygo/client"
)

// AuthListener receives a Hocuspocus server's answer to the token. It is
// separate from Listener so that existing Listener implementations keep
// compiling; set it with Client.SetAuthListener. Each call runs on a
// goroutine of its own, so calling Close from it is safe; dispatch to
// the main thread before touching UI.
type AuthListener interface {
	// OnAuthenticated fires when the server accepts the token, on every
	// connect, with the scope it granted: "read-write" or "readonly".
	OnAuthenticated(scope string)
	// OnPermissionDenied fires when the server refuses the token, with
	// its reason. The client tries again after at least 30 seconds with
	// the token it then has (SetToken); call Close to stop (safe here).
	OnPermissionDenied(reason string)
}

// SetHocuspocus makes the client speak the Hocuspocus protocol (for
// @hocuspocus/server 2, 3 and 4), sending token to the server's
// onAuthenticate. The URL is then the server's, without the document
// name. Call before Connect; a no-op after.
func (c *Client) SetHocuspocus(token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.inner == nil {
		c.hocuspocus = true
		c.token = token
	}
}

// SetToken replaces the token; the client sends it at the next Auth: the
// next connect, or when the server asks for the token again.
func (c *Client) SetToken(token string) {
	c.mu.Lock()
	c.token = token
	c.mu.Unlock()
}

// SetAuthListener registers the listener for the server's answer to the
// token. Call before Connect; calls after Connect are ignored.
func (c *Client) SetAuthListener(l AuthListener) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.inner == nil {
		c.auth = l
	}
}

// Scope is the scope the Hocuspocus server granted ("read-write" or
// "readonly"), or "" when not connected.
func (c *Client) Scope() string {
	c.mu.Lock()
	inner := c.inner
	c.mu.Unlock()
	if inner == nil {
		return ""
	}
	return inner.Scope()
}

// Unsynced is the number of this client's updates the Hocuspocus server
// has not yet confirmed applying; 0 when it has everything. Always 0
// outside Hocuspocus mode.
func (c *Client) Unsynced() int {
	c.mu.Lock()
	inner := c.inner
	c.mu.Unlock()
	if inner == nil {
		return 0
	}
	return inner.Unsynced()
}

// SendStateless sends a Hocuspocus stateless message to the server's
// onStateless hook.
func (c *Client) SendStateless(payload string) error {
	c.mu.Lock()
	inner := c.inner
	c.mu.Unlock()
	if inner == nil {
		return errors.New("gomobile: not connected")
	}
	return inner.SendStateless(payload)
}

// hocusOptions sets the Hocuspocus fields of opts. Called by Connect
// with c.mu held.
func (c *Client) hocusOptions(opts *client.Options) {
	if !c.hocuspocus {
		return
	}
	opts.Hocuspocus = true
	opts.TokenFunc = func(context.Context) (string, error) {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.token, nil
	}
	a := c.auth
	if a == nil {
		return
	}
	// Off the client's goroutines: Close waits for those, and an app
	// that closes on a refusal calls it from here.
	opts.OnAuthenticated = func(scope string) { go a.OnAuthenticated(scope) }
	onError := opts.OnError
	opts.OnError = func(err error) {
		if errors.Is(err, client.ErrPermissionDenied) {
			go a.OnPermissionDenied(strings.TrimPrefix(err.Error(), client.ErrPermissionDenied.Error()+": "))
		}
		if onError != nil {
			onError(err)
		}
	}
}
