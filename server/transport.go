package server

import (
	"bufio"
	"net"
	"net/http"
	"time"

	"github.com/coder/websocket"
)

// defaultCloseHandshakeLimit bounds a close the server starts. coder/websocket's
// Close writes the close frame and waits for the peer's, five seconds each,
// but when the peer has announced a data frame before its own close, Close
// reads that frame's payload with no deadline: a peer that withholds the
// payload holds Close, and everything that waits for it, forever. After this
// long the server cuts the transport under the close.
const defaultCloseHandshakeLimit = 15 * time.Second

// hijackRecorder hands the ResponseWriter to websocket.Accept and keeps the
// net.Conn that Accept hijacks, so the server can always cut the transport.
type hijackRecorder struct {
	http.ResponseWriter
	hijacker http.Hijacker
	conn     net.Conn
}

// recordHijack wraps w when it can be hijacked, looking through Unwrap as
// websocket.Accept does. Otherwise it returns w unwrapped, and Accept refuses
// it as before.
func recordHijack(w http.ResponseWriter) (http.ResponseWriter, *hijackRecorder) {
	rw := w
	for {
		switch t := rw.(type) {
		case http.Hijacker:
			rec := &hijackRecorder{ResponseWriter: w, hijacker: t}
			return rec, rec
		case interface{ Unwrap() http.ResponseWriter }:
			rw = t.Unwrap()
		default:
			return w, nil
		}
	}
}

// Hijack hijacks the connection and records it.
func (h *hijackRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	c, brw, err := h.hijacker.Hijack()
	h.conn = c
	return c, brw, err
}

// WriteHeaderNow forwards to a ResponseWriter that holds its header until
// asked (gin's): websocket.Accept calls it before it hijacks.
func (h *hijackRecorder) WriteHeaderNow() {
	if w, ok := h.ResponseWriter.(interface{ WriteHeaderNow() }); ok {
		w.WriteHeaderNow()
	}
}

// Unwrap returns the wrapped ResponseWriter, for http.ResponseController.
func (h *hijackRecorder) Unwrap() http.ResponseWriter { return h.ResponseWriter }

// closeWithin closes ws with code and reason, and closes raw, the hijacked
// transport, if that takes longer than limit. A nil raw (a connection whose
// transport is unknown) closes without the bound.
func closeWithin(ws *websocket.Conn, raw net.Conn, limit time.Duration, code websocket.StatusCode, reason string) {
	if raw != nil {
		cut := time.AfterFunc(limit, func() { _ = raw.Close() })
		defer cut.Stop()
	}
	_ = ws.Close(code, reason)
}
