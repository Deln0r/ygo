package server

import (
	"testing"
	"time"

	syncpkg "github.com/Deln0r/ygo/internal/sync"
)

func TestHocuspocusSharedQueueStats(t *testing.T) {
	q := newSendQueue(10, 1000, time.Second)
	for _, key := range []string{"doc", "doc\x00session", "different"} {
		c := &conn{route: syncpkg.EncodeRoute(key), queue: q}
		q.pushFanout(c.routed(syncpkg.EncodeStateless("hello")))
	}
	for _, key := range []string{"doc", "doc\x00session", "different"} {
		n, size := q.backlogForRoute(syncpkg.EncodeRoute(key))
		if n != 1 || size != int64(len(syncpkg.EncodeRoute(key))+len(syncpkg.EncodeStateless("hello"))) {
			t.Fatalf("%q: %d messages, %d bytes", key, n, size)
		}
	}
}
