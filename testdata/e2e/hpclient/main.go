// Command hpclient is the Go side of hocuspocus.mjs: a ygo client in
// Hocuspocus mode, driven by JSON lines on stdin, reporting what happens as
// JSON lines on stdout.
//
//	hpclient -url ws://127.0.0.1:PORT -doc NAME [-token T] [-awareness-timeout MS]
//
// Commands: set {key,value} on the map "m"; get {key}; presence {state};
// peer {id}; stateless {payload}; status; close. Events: ready, synced,
// authenticated, stateless, error (with kind denied, closed, refused,
// silent or other), value, peer, status, closed.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/Deln0r/ygo/client"
	"github.com/Deln0r/ygo/internal/types"
)

var (
	outMu sync.Mutex
	enc   = json.NewEncoder(os.Stdout)
)

func emit(v map[string]any) {
	outMu.Lock()
	defer outMu.Unlock()
	_ = enc.Encode(v)
}

func kind(err error) string {
	switch {
	case errors.Is(err, client.ErrPermissionDenied):
		return "denied"
	case errors.Is(err, client.ErrDocumentClosed):
		return "closed"
	case errors.Is(err, client.ErrUpdateRefused):
		return "refused"
	case errors.Is(err, client.ErrServerSilent):
		return "silent"
	}
	return "other"
}

type command struct {
	Op      string `json:"op"`
	Key     string `json:"key"`
	Value   string `json:"value"`
	State   string `json:"state"`
	ID      uint64 `json:"id"`
	Payload string `json:"payload"`
}

func main() {
	url := flag.String("url", "", "Hocuspocus server URL")
	docName := flag.String("doc", "", "document name")
	token := flag.String("token", "", "token")
	awMS := flag.Int("awareness-timeout", 0, "AwarenessTimeout in ms (0: default)")
	flag.Parse()

	c, err := client.New(client.Options{
		URL: *url, DocName: *docName, Hocuspocus: true, Token: *token,
		AwarenessTimeout: time.Duration(*awMS) * time.Millisecond,
		MinBackoff:       100 * time.Millisecond, MaxBackoff: 500 * time.Millisecond,
		OnSynced:        func(v bool) { emit(map[string]any{"ev": "synced", "v": v}) },
		OnError:         func(err error) { emit(map[string]any{"ev": "error", "kind": kind(err), "msg": err.Error()}) },
		OnAuthenticated: func(scope string) { emit(map[string]any{"ev": "authenticated", "scope": scope}) },
		OnStateless:     func(p string) { emit(map[string]any{"ev": "stateless", "payload": p}) },
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	m := types.NewMap(c.Doc().Branch("m"))
	if err := c.Connect(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	// The presence ID, the key the harness looks the client up by; the
	// document's ID can change (Doc.ClientID).
	emit(map[string]any{"ev": "ready", "id": c.Awareness().ClientID()})

	in := bufio.NewScanner(os.Stdin)
	for in.Scan() {
		var cmd command
		if err := json.Unmarshal(in.Bytes(), &cmd); err != nil {
			emit(map[string]any{"ev": "bad-command", "msg": err.Error()})
			continue
		}
		switch cmd.Op {
		case "set":
			txn := c.Doc().WriteTxn()
			m.Set(txn, cmd.Key, cmd.Value)
			txn.Commit()
		case "get":
			rtxn := c.Doc().ReadTxn()
			v := m.Get(cmd.Key)
			rtxn.Close()
			emit(map[string]any{"ev": "value", "key": cmd.Key, "value": v})
		case "presence":
			c.SetAwarenessState([]byte(cmd.State))
		case "peer":
			var state any
			if s, ok := c.Awareness().States()[cmd.ID]; ok {
				state = string(s)
			}
			emit(map[string]any{"ev": "peer", "id": cmd.ID, "state": state})
		case "stateless":
			if err := c.SendStateless(cmd.Payload); err != nil {
				emit(map[string]any{"ev": "error", "kind": "other", "msg": err.Error()})
			}
		case "status":
			emit(map[string]any{"ev": "status", "synced": c.Synced(), "unsynced": c.Unsynced(), "scope": c.Scope()})
		case "close":
			_ = c.Close()
			emit(map[string]any{"ev": "closed"})
			return
		}
	}
	_ = c.Close()
}
