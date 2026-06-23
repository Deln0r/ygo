// Command yserve is a self-hosted Yjs sync server in a single static
// binary: a drop-in replacement for a Hocuspocus deployment with no
// Node runtime, no Redis, and no CGO.
//
// It speaks the Hocuspocus message envelope (Sync, Awareness,
// QueryAwareness, Auth, Stateless, BroadcastStateless, Close,
// SyncStatus), so existing @hocuspocus/provider and y-websocket
// clients connect unchanged. SQLite persistence and periodic document
// versioning are built in.
//
// Usage:
//
//	yserve [-addr :8080] [-store path/to/ygo.db]
//	       [-version-interval 10m] [-keep-versions 10]
//	       [-read-limit BYTES] [-awareness-timeout 30s]
//	       [-max-awareness-clients 4096]
//
// Without -store the server runs purely in-memory; documents are lost
// when their last connection disconnects. With -store every applied
// update is persisted to a pure-Go SQLite database and document
// history is loaded on first connect after a restart.
//
// With -version-interval > 0 (requires -store), every document that
// changed since the previous interval is captured as a named version;
// -keep-versions bounds the history per document (0 keeps everything).
// Versions survive log compaction and can be listed, loaded, and
// restored programmatically via the persist package.
//
// -read-limit raises the maximum WebSocket frame size the server will
// read (default 32 KiB); raise it for large documents whose sync frame
// exceeds the default, or pass -1 for unlimited. -awareness-timeout and
// -max-awareness-clients tune the presence layer's silent-client
// eviction window and per-room client cap.
//
// Mount point: documents are addressed by the URL path. A client
// connecting to ws://host:8080/my-doc operates on docName "my-doc",
// matching y-websocket's convention.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Deln0r/ygo/persist"
	"github.com/Deln0r/ygo/persist/sqlite"
	"github.com/Deln0r/ygo/server"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	storePath := flag.String("store", "", "SQLite database path for persistence (empty = in-memory)")
	versionInterval := flag.Duration("version-interval", 0, "capture a version of each changed document at this interval (0 = off; requires -store)")
	keepVersions := flag.Int("keep-versions", 10, "keep at most N versions per document when auto-versioning (0 = keep all)")
	readLimit := flag.Int64("read-limit", 0, "max WebSocket message size in bytes per client frame (0 = 32768 default; -1 = unlimited)")
	awarenessTimeout := flag.Duration("awareness-timeout", 0, "evict a presence entry after it is silent this long (0 = 30s default)")
	maxAwarenessClients := flag.Int("max-awareness-clients", 0, "cap distinct presence clients per room (0 = 4096 default; -1 = unlimited)")
	maxConnsPerDoc := flag.Int("max-conns-per-doc", 0, "cap simultaneous WebSocket connections per document (0 = 4096 default; -1 = unlimited)")
	flag.Parse()

	var store persist.Store
	if *storePath != "" {
		s, err := sqlite.Open(*storePath)
		if err != nil {
			log.Fatalf("yserve: open store %q: %v", *storePath, err)
		}
		defer s.Close()
		store = s
		log.Printf("yserve: persistence enabled (sqlite at %s)", *storePath)
	} else {
		log.Printf("yserve: in-memory only (pass -store to persist)")
		if *versionInterval > 0 {
			log.Fatalf("yserve: -version-interval requires -store")
		}
	}
	if *versionInterval > 0 {
		log.Printf("yserve: auto-versioning every %s (keep %d)", *versionInterval, *keepVersions)
	}

	srv := server.New(server.Options{
		Store:               store,
		OriginPatterns:      []string{"*"}, // dev-friendly; tighten in prod
		VersionInterval:     *versionInterval,
		KeepVersions:        *keepVersions,
		ReadLimit:           *readLimit,
		AwarenessTimeout:    *awarenessTimeout,
		MaxAwarenessClients: *maxAwarenessClients,
		MaxConnsPerDoc:      *maxConnsPerDoc,
	})

	httpSrv := &http.Server{
		Addr:    *addr,
		Handler: srv.Handler(),
	}

	// Graceful shutdown on SIGINT/SIGTERM.
	idleConnsClosed := make(chan struct{})
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		log.Printf("yserve: shutting down")

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(ctx); err != nil {
			log.Printf("yserve: HTTP shutdown: %v", err)
		}
		if err := srv.Close(ctx); err != nil {
			log.Printf("yserve: store flush: %v", err)
		}
		close(idleConnsClosed)
	}()

	log.Printf("yserve: listening on %s", *addr)
	if err := httpSrv.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("yserve: %v", err)
	}
	<-idleConnsClosed
	log.Printf("yserve: stopped")
}
