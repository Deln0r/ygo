//go:build js

package client

import (
	"context"

	"github.com/coder/websocket"
)

// dial opens a session's connection. There is no liveness check on js:
// the browser's WebSocket has no ping, and coder/websocket's Ping returns
// at once there.
func (c *Client) dial(ctx context.Context, url string) (*websocket.Conn, *liveness, error) {
	conn, _, err := websocket.Dial(ctx, url, nil)
	return conn, nil, err
}

func (c *Client) watch(ctx context.Context, _ *websocket.Conn, _ *liveness) bool {
	<-ctx.Done()
	return false
}
