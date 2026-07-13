# offline-first example

Demonstrates the ygo client's **offline-first** persistence: with a
`LocalStore`, the document loads from disk before any network, stays
editable with no server reachable, and carries offline edits up to the
server on the next successful connect.

## Run

```sh
# No server reachable: the log persists locally across restarts.
go run ./examples/offline-first -store notes.db -add "first note"
go run ./examples/offline-first -store notes.db -add "second note"
go run ./examples/offline-first -store notes.db              # lists both

# With a server (e.g. the collab-server example), offline edits sync up:
go run ./examples/offline-first -store notes.db -url ws://localhost:8080/collab
```

## What it shows

`client.Connect` loads the `LocalStore` synchronously, so the document is
usable immediately even with no network. Local edits and applied remote
updates are persisted to disk (pure-Go SQLite), and on the next connect
the handshake carries any edits made while offline up to the server. This
is the building block for the mobile SDK's `EnableOfflineStore`, and for
bots, CLI tools, and server-side agents that must keep working through a
network partition.

Reads off the shared document use a `ReadTxn` because a live sync
connection applies remote updates on its own goroutine.
