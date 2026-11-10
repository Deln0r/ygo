// Command yserve is a self-hosted Yjs sync server in one binary, with
// no Node runtime and no Redis; built with CGO_ENABLED=0 it is fully
// static.
//
// It speaks the y-websocket protocol, so y-websocket clients connect
// unchanged. @hocuspocus/provider cannot connect: Hocuspocus prefixes
// every frame with the document name, which yserve does not parse.
// SQLite persistence and periodic document versioning are built in.
//
// Usage:
//
//	yserve [-addr :8080] [-store path/to/ygo.db]
//	       [-version-interval 10m] [-keep-versions 10]
//	       [-read-limit BYTES] [-awareness-timeout 30s]
//	       [-max-awareness-clients 4096] [-path-prefix /]
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
// -read-limit sets the maximum WebSocket message the server will read
// (default 4 MiB); raise it for documents whose sync message exceeds the
// default, or pass -1 for unlimited. -awareness-timeout and
// -max-awareness-clients tune the presence layer's silent-client
// eviction window and per-room client cap.
//
// Mount point: documents are addressed by the URL path, every segment
// kept. A client connecting to ws://host:8080/team-a/doc operates on
// docName "team-a/doc", as with y-websocket's reference server. Behind a
// proxy that forwards a path prefix, pass it as -path-prefix: with
// -path-prefix /ws/, ws://host/ws/team-a/doc is "team-a/doc" again.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path"
	"strings"
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
	readLimit := flag.Int64("read-limit", 0, "max WebSocket message size in bytes per client frame (0 = 4 MiB default; -1 = unlimited)")
	pathPrefix := flag.String("path-prefix", "/", "URL path the documents live under; the rest of the path is the document name")
	awarenessTimeout := flag.Duration("awareness-timeout", 0, "evict a presence entry after it is silent this long (0 = 30s default)")
	maxAwarenessClients := flag.Int("max-awareness-clients", 0, "cap distinct presence clients per room (0 = 4096 default; -1 = unlimited)")
	maxConnsPerDoc := flag.Int("max-conns-per-doc", 0, "cap simultaneous WebSocket connections per document (0 = 4096 default; -1 = unlimited)")
	maxDocs := flag.Int("max-docs", 0, "cap distinct documents held in memory at once (0 = unlimited; guards against many fabricated docNames)")
	maxConns := flag.Int("max-conns", 0, "cap total simultaneous WebSocket connections across all documents (0 = unlimited)")
	writeTimeout := flag.Duration("write-timeout", 0, "close a client a write to it has waited this long, or whose queued message has (0 = 10s default)")
	maxSendQueue := flag.Int("max-send-queue", 0, "close a client with this many room messages queued to it (0 = 4096 default; -1 = unlimited)")
	maxSendQueueBytes := flag.Int64("max-send-queue-bytes", 0, "close a client with this many bytes of room messages queued to it (0 = 16 MiB or 4x read-limit; -1 = unlimited)")
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
		MaxDocs:             *maxDocs,
		MaxConns:            *maxConns,
		WriteTimeout:        *writeTimeout,
		MaxSendQueue:        *maxSendQueue,
		MaxSendQueueBytes:   *maxSendQueueBytes,
	})

	// At the root the handler serves every path itself (the whole path is
	// the document name); under a prefix a ServeMux strips it, and the
	// ServeMux redirects paths with "//" or dot segments.
	trimmed := "/" + strings.Trim(*pathPrefix, "/")
	if strings.ContainsAny(*pathPrefix, "{} \t") || path.Clean(trimmed) != trimmed {
		log.Fatalf("yserve: -path-prefix %q: give a plain path such as /ws/, without spaces, braces, // or dot segments", *pathPrefix)
	}
	handler := srv.Handler()
	if trimmed != "/" {
		mux := http.NewServeMux()
		mux.Handle(trimmed+"/", srv.Handler())
		handler = mux
	}

	httpSrv := &http.Server{
		Addr:    *addr,
		Handler: handler,
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
