# collab-client example

A runnable example of the ygo **Go-native sync client**
([`client`](https://pkg.go.dev/github.com/Deln0r/ygo/client)): it connects
to a ygo (or any y-websocket) server, observes a shared array, appends one
entry, and logs every change — local and remote — as the document
converges. A Go client that speaks the y-websocket protocol is a
distinguishing feature; most CRDT stacks only ship a browser client.

## Run

Against the sibling server example ([`../collab-server`](../collab-server)):

```sh
go run ./examples/collab-server &
go run ./examples/collab-client -url ws://localhost:8080/collab -doc room1 -name alice
go run ./examples/collab-client -url ws://localhost:8080/collab -doc room1 -name bob
```

Each client appends its `-name` to the shared `items` array and logs the
others' entries as they arrive. It also speaks to the public demo:

```sh
go run ./examples/collab-client -url wss://ygo.deln0r.com/ws -doc my-room -name alice
```

## What it demonstrates

- `client.New` + `Connect` / `Close`: a reconnecting sync session for one
  document, safe for concurrent use.
- `OnSynced` / `OnError`: handshake and connection-error callbacks.
- Editing through the public API on `Client.Doc()` (`ygo.NewArray` +
  `WriteTxn` / `Push` / `Commit`), which syncs to the server and peers.
- `Array.Observe`: a change feed that fires for both local edits and
  applied remote updates.

For offline-first behavior, set `client.Options.LocalStore` to a
`persist.Store` (the pure-Go `persist/sqlite` works on-device); the client
then loads and persists locally and carries offline edits up on reconnect.

## Reading the document safely

The client applies remote updates on its own goroutine, so inspecting the
document from another goroutine (a bare `array.Len()`, `map.Get`, …) races
that writer. Wrap reads in a read transaction, which serializes against the
apply, as `readLen` in the test shows:

```go
rtxn := c.Doc().ReadTxn()
n := items.Len()
rtxn.Close()
```

Prefer `Observe` (as in `main.go`) for reacting to changes: its callback
runs on the mutating goroutine, so it needs no extra locking.
