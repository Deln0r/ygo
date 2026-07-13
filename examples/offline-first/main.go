// Command offline-first demonstrates the ygo client's offline-first
// persistence. With a LocalStore the document loads from disk before any
// network, stays editable with no server reachable, and carries offline
// edits up to the server on the next successful connect.
//
// Run it repeatedly against the same -store to watch the log persist
// across restarts with no server at all:
//
//	go run ./examples/offline-first -store notes.db -add "first note"
//	go run ./examples/offline-first -store notes.db -add "second note"
//	go run ./examples/offline-first -store notes.db            # lists both
//
// Point -url at a live ygo/y-websocket server and the accumulated offline
// edits sync up on connect (the handshake carries the local state).
package main

import (
	"context"
	"flag"
	"fmt"
	"log"

	"github.com/Deln0r/ygo"
	"github.com/Deln0r/ygo/client"
	"github.com/Deln0r/ygo/persist"
	"github.com/Deln0r/ygo/persist/sqlite"
)

func main() {
	storePath := flag.String("store", "notes.db", "sqlite path for the local store")
	url := flag.String("url", "ws://127.0.0.1:9", "server URL (the default port is unreachable, i.e. offline)")
	docName := flag.String("doc", "notes", "document name")
	add := flag.String("add", "", "append this line to the shared log")
	flag.Parse()

	store, err := sqlite.Open(*storePath)
	if err != nil {
		log.Fatalf("open local store: %v", err)
	}
	defer store.Close()

	entries, err := appendAndList(store, *url, *docName, *add)
	if err != nil {
		log.Fatalf("run: %v", err)
	}

	fmt.Printf("log (%d entries):\n", len(entries))
	for i, e := range entries {
		fmt.Printf("  %d: %s\n", i, e)
	}
}

// appendAndList opens a client on the given local store, optionally
// appends an entry to the shared "log" array, and returns the current
// log. The client is closed before returning, which flushes the final
// state to the store (so the next run loads it). Connect loads the store
// synchronously, so this works with no server reachable; a reachable
// server also receives the accumulated edits via the connect handshake.
func appendAndList(store persist.Store, url, docName, entry string) ([]string, error) {
	c, err := client.New(client.Options{
		URL:        url,
		DocName:    docName,
		LocalStore: store,
		OnSynced: func(synced bool) {
			if synced {
				log.Println("synced with server")
			}
		},
	})
	if err != nil {
		return nil, err
	}
	if err := c.Connect(context.Background()); err != nil {
		return nil, err
	}
	defer c.Close() // final persist + flush to the local store

	logArr := ygo.NewArray(c.Doc(), "log")
	if entry != "" {
		txn := c.Doc().WriteTxn()
		logArr.Push(txn, entry)
		txn.Commit()
	}

	// Read under a ReadTxn: a live sync connection applies remote updates
	// on its own goroutine, so reads off the shared doc must be guarded.
	rtxn := c.Doc().ReadTxn()
	defer rtxn.Close()
	out := make([]string, 0, logArr.Len())
	for i := uint64(0); i < logArr.Len(); i++ {
		if s, ok := logArr.Get(i).(string); ok {
			out = append(out, s)
		}
	}
	return out, nil
}
