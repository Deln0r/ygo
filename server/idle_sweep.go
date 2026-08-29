package server

import (
	"context"
	"time"
)

// startIdleSweep launches the ticker goroutine that evicts documents parked
// by Options.DocIdleTimeout once they have been idle past the timeout. It is
// a no-op when idle keep-warm is off (the zero default), so servers that
// never park pay nothing.
func (s *Server) startIdleSweep() {
	if s.opts.DocIdleTimeout <= 0 {
		return
	}
	interval := s.opts.DocIdleTimeout / 4
	if interval < time.Second {
		interval = time.Second
	}
	s.idleStop = make(chan struct{})
	s.idleDone = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	s.idleCancel = cancel
	go func() {
		defer close(s.idleDone)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-s.idleStop:
				return
			case <-ticker.C:
				s.sweepIdle(ctx)
			}
		}
	}()
}

// stopIdleSweep terminates the eviction loop and waits for an in-flight
// sweep to finish. Safe to call when the sweep never started.
func (s *Server) stopIdleSweep() {
	if s.idleStop == nil {
		return
	}
	close(s.idleStop)
	// Cancel the sweep's eviction context BEFORE waiting: an in-flight
	// eviction blocked in Store.Flush aborts instead of pinning Close past
	// its deadline behind a wedged store.
	s.idleCancel()
	<-s.idleDone
	s.idleStop = nil
}

// sweepIdle evicts every document parked longer than DocIdleTimeout. The
// registry mutation happens under docsMu with a connsMu recheck (mirroring
// releaseConn's eviction discipline: a concurrent admission clears idleSince
// under docsMu, so an expired-looking document that just gained a connection
// is skipped); the Store flush and backplane unsubscribe happen off-lock.
func (s *Server) sweepIdle(ctx context.Context) {
	cutoff := time.Now().Add(-s.opts.DocIdleTimeout)
	var expired []*docState
	s.docsMu.Lock()
	for name, st := range s.docs {
		if st.idleSince.IsZero() || st.idleSince.After(cutoff) {
			continue
		}
		st.connsMu.Lock()
		if len(st.conns) == 0 {
			delete(s.docs, name)
			expired = append(expired, st)
		}
		st.connsMu.Unlock()
	}
	s.docsMu.Unlock()
	for _, st := range expired {
		s.finishEviction(ctx, st)
	}
}

// evictOldestIdleLocked enforces Options.MaxIdleDocs: when more than limit
// documents are parked, the least-recently-idle one is removed from the
// registry and returned for the caller to finish evicting off-lock. The
// caller MUST hold docsMu. Parking exceeds the bound by at most one document
// at a time, so evicting one restores the invariant.
func (s *Server) evictOldestIdleLocked(limit int) *docState {
	idle := 0
	var oldest *docState
	for _, st := range s.docs {
		if st.idleSince.IsZero() {
			continue
		}
		idle++
		if oldest == nil || st.idleSince.Before(oldest.idleSince) {
			oldest = st
		}
	}
	if idle <= limit || oldest == nil {
		return nil
	}
	delete(s.docs, oldest.name)
	return oldest
}

// finishEviction performs the off-lock half of removing a document from the
// registry: stop its backplane subscription and write the final Store
// checkpoint. The registry delete must already have happened under docsMu.
// Flush is best-effort, exactly as in releaseConn's eviction: the update log
// is intact in the Store either way.
func (s *Server) finishEviction(ctx context.Context, st *docState) {
	if st.backplaneUnsub != nil {
		st.backplaneUnsub()
	}
	if s.opts.Store != nil {
		_ = s.opts.Store.Flush(ctx, st.name)
	}
}
