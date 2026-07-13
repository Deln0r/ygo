# Examples

Runnable examples for embedding and using ygo. Each is a `go run`-able
program with its own README.

| Example | What it shows |
|---|---|
| [collab-server](collab-server) | Embed the WebSocket sync server and wire its hooks (`OnConnect`, `ReadOnly`, `OnChange`, `OnLoadDocument`), resource caps, a SQLite store, and a `/stats` endpoint |
| [collab-client](collab-client) | A Go-native sync client: connect, observe remote changes, edit, converge |
| [offline-first](offline-first) | Client-side offline persistence with a `LocalStore`: usable with no network, edits survive restarts and sync up on reconnect |

The core CRDT API also has runnable, output-verified examples that render
on [pkg.go.dev](https://pkg.go.dev/github.com/Deln0r/ygo#pkg-examples)
(Map, Array, Text, two-document sync, UndoManager).
