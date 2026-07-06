# collab-server example

A runnable example of **embedding** the ygo WebSocket sync server in your
own Go backend and wiring its library-only extension points. It is
documentation, not a product: [`cmd/yserve`](../../cmd/yserve) is the
batteries-included CLI server driven by flags; this example shows what the
[`server`](https://pkg.go.dev/github.com/Deln0r/ygo/server) package
exposes to code that mounts it, which the CLI cannot express.

## Run

```sh
go run ./examples/collab-server              # in-memory
go run ./examples/collab-server -store x.db  # sqlite-backed (survives restart)
```

Then connect any [y-websocket](https://github.com/yjs/y-websocket) client:

```
ws://localhost:8080/collab/<docName>            # editor
ws://localhost:8080/collab/<docName>?mode=view  # read-only viewer
```

and check load:

```sh
curl http://localhost:8080/stats   # {"Documents":N,"Connections":M}
```

## What it demonstrates

Everything is wired in `newServer` and documented in full on
`server.Options`:

| Piece | Option | What it shows |
|---|---|---|
| Persistence | `Store` | sqlite-backed history that survives restart (WAL, concurrency-safe) |
| Resource caps | `MaxConnsPerDoc`, `MaxDocs` | bound what one peer can pin in memory |
| Connection gate | `OnConnect` / `OnDisconnect` | request-aware accept/reject (headers, cookies, IP) and lifecycle logging |
| Read-only viewers | `ReadOnly` | `?mode=view` receives updates and shows a cursor but cannot write |
| Change side effects | `OnChange` | react to every applied edit (index, webhook, mirror) |
| First-load seeding | `OnLoadDocument` | seed a fresh document's initial content with a stable-ID update |
| Live load | `Server.Stats()` | a `/stats` JSON endpoint, mounted beside the collab handler |

The collab handler is mounted at `/collab/` and the stats endpoint at
`/stats`, so the stats path never collides with a document name.

The welcome seed is built **once** with a fixed `ClientID` so its item IDs
are stable across reloads: `OnLoadDocument` re-applies it on every first
load, and stable IDs make that idempotent without persisting the seed.
