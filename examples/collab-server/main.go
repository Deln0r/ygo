// Command collab-server is a runnable example of embedding the ygo
// WebSocket sync server in your own Go backend and wiring its
// library-only extension points: connection lifecycle hooks, read-only
// viewers, an on-change side effect, first-load seeding, resource caps,
// and a live stats endpoint. It is documentation, not a product — yserve
// (cmd/yserve) is the batteries-included CLI server; this shows what the
// server package exposes to code that embeds it.
//
// Run:
//
//	go run ./examples/collab-server              # in-memory
//	go run ./examples/collab-server -store x.db  # sqlite-backed
//
// Then point any y-websocket client at
//
//	ws://localhost:8080/collab/<docName>            # editor
//	ws://localhost:8080/collab/<docName>?mode=view  # read-only viewer
//
// and GET http://localhost:8080/stats for current load.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Deln0r/ygo"
	"github.com/Deln0r/ygo/persist"
	"github.com/Deln0r/ygo/persist/sqlite"
	"github.com/Deln0r/ygo/server"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	storePath := flag.String("store", "", "sqlite path (empty = in-memory)")
	flag.Parse()

	var store persist.Store
	if *storePath != "" {
		s, err := sqlite.Open(*storePath)
		if err != nil {
			log.Fatalf("open store: %v", err)
		}
		defer s.Close()
		store = s
	}

	srv, mux := newServer(store)

	httpSrv := &http.Server{Addr: *addr, Handler: mux}
	go func() {
		log.Printf("collab-server on %s (ws %s/collab/<doc>, GET %s/stats)", *addr, *addr, *addr)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("serve: %v", err)
		}
	}()

	// Graceful shutdown: stop accepting, drain HTTP, flush documents.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
	_ = srv.Close(ctx)
	log.Println("stopped")
}

// newServer wires the example server and a mux serving the collab
// endpoint plus a JSON /stats endpoint. It is split out from main so the
// smoke test can exercise it. Every option below is documented in detail
// on server.Options.
func newServer(store persist.Store) (*server.Server, *http.ServeMux) {
	// A stable seed applied to every new document on first load. It is
	// built ONCE (fixed ClientID) so its item IDs are stable across
	// reloads — Options.OnLoadDocument requires that for idempotent
	// re-seeding without persistence.
	welcome := buildWelcomeSeed()

	srv := server.New(server.Options{
		Store:          store,
		OriginPatterns: []string{"*"}, // dev only; list your origins in production

		// Bound what a single peer can pin in memory.
		MaxConnsPerDoc: 512,
		MaxDocs:        10000,

		// OnConnect is a request-aware gate: return an error to reject
		// (an IP allow-list, a cookie or JWT check, ...). Here it logs.
		OnConnect: func(connID, docName string, r *http.Request) error {
			log.Printf("connect %s doc=%s from=%s", connID, docName, r.RemoteAddr)
			return nil
		},
		OnDisconnect: func(connID, docName string) {
			log.Printf("disconnect %s doc=%s", connID, docName)
		},

		// A "?mode=view" connection is read-only: it receives updates and
		// shows a cursor but cannot write to the document.
		ReadOnly: func(_ string, r *http.Request) bool {
			return r.URL.Query().Get("mode") == "view"
		},

		// OnChange is a side effect on every applied edit: index it, fire
		// a webhook, mirror it elsewhere. Here it logs the update size.
		OnChange: func(docName string, update []byte) {
			log.Printf("change doc=%s bytes=%d", docName, len(update))
		},

		// OnLoadDocument seeds a fresh document's initial content on its
		// first load, on top of any stored history.
		OnLoadDocument: func(_ string) ([]byte, error) {
			return welcome, nil
		},
	})

	mux := http.NewServeMux()
	mux.Handle("/collab/", srv.Handler())
	mux.HandleFunc("/stats", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(srv.Stats())
	})
	return srv, mux
}

// buildWelcomeSeed builds the initial content seeded into every fresh
// document: one "Welcome" entry. The fixed ClientID keeps the seed's item
// IDs stable across reloads, which OnLoadDocument requires.
func buildWelcomeSeed() []byte {
	d := ygo.NewDocWithOptions(ygo.Options{ClientID: 1})
	arr := ygo.NewArray(d, "items")
	txn := d.WriteTxn()
	arr.Push(txn, "Welcome")
	txn.Commit()
	return ygo.EncodeStateAsUpdate(d)
}
