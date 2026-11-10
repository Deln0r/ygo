package client

import (
	"sync/atomic"
	"time"
)

// liveness holds what one session's connection did, for the liveness
// check: when a byte was last read, and when the kernel last took another
// writeMark bytes of what the client wrote. Times are offsets from base, a
// reading of the monotonic clock, so a step of the wall clock changes
// nothing.
type liveness struct {
	base      time.Time
	lastRead  atomic.Int64
	lastMark  atomic.Int64
	unmarked  atomic.Int64 // bytes written since the last mark
	pongCount *atomic.Int64
}

// writeMark is how many bytes the kernel must take for a write to count as
// progress. A dead path still takes small writes (a presence renewal, a
// keystroke) into its send buffer, so they must not keep it alive; a large
// write that the kernel takes piece by piece is a path that delivers.
const writeMark = 64 << 10

func newLiveness(pongs *atomic.Int64) *liveness {
	return &liveness{base: time.Now(), pongCount: pongs}
}

func (l *liveness) now() int64 { return int64(time.Since(l.base)) }

func (l *liveness) read() { l.lastRead.Store(l.now()) }

func (l *liveness) wrote(n int) {
	if l.unmarked.Add(int64(n)) >= writeMark {
		l.unmarked.Store(0)
		l.lastMark.Store(l.now())
	}
}
