# yserve — self-hosted Yjs server in a single binary

yserve is a self-hosted Yjs sync backend in one binary. It speaks the
`y-websocket` protocol, so `y-websocket` clients connect to it unchanged
(checked with y-websocket 2.1 and 3.1). SQLite persistence and periodic
document versioning are built in.

**`@hocuspocus/provider` does not work with yserve.** Hocuspocus starts
every frame with the document name, which yserve does not parse. (The Go
client speaks Hocuspocus to Hocuspocus servers with
`client.Options.Hocuspocus`.) See [Limits](#limits) before deploying.

**No Node. No Redis. No CGO.** Pure Go from the WebSocket accept loop
down to the SQLite pages (via `modernc.org/sqlite`), which is why the
Docker image is `FROM scratch` and the binary cross-compiles to the
targets `modernc.org/sqlite` supports (not wasm, AIX, DragonFly,
illumos, Plan 9, Solaris and some BSD and Linux ports).

## Quick start

```bash
CGO_ENABLED=0 go install github.com/Deln0r/ygo/cmd/yserve@latest   # fully static on Linux
yserve -addr :8080 -store data.db
```

or with Docker:

```bash
docker build -t yserve https://github.com/Deln0r/ygo.git
docker run -p 8080:8080 -v yserve-data:/data yserve
```

Point a `y-websocket` client at it:

```js
import * as Y from 'yjs'
import { WebsocketProvider } from 'y-websocket'

const doc = new Y.Doc()
const provider = new WebsocketProvider('ws://localhost:8080', 'my-document', doc)
```

Documents are addressed by the URL path, every segment kept, as in
y-websocket's reference server: `ws://localhost:8080/team-a/doc` is the
document `team-a/doc`. Behind a proxy that forwards a path prefix, pass it
as `-path-prefix`, so that with `-path-prefix /ws/`
`ws://host/ws/team-a/doc` is `team-a/doc` again.

## Limits

- **Hocuspocus.** `@hocuspocus/provider` (checked with 2.15, 3.4 and
  4.8) cannot connect: its frames start with the document name.
- **Message size.** A client message larger than `-read-limit` (4 MiB
  by default) closes the connection with 1009, and the client
  reconnects and resends it in a loop. The first sync of a large
  document is one message. Set `-read-limit` above your largest
  document.

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `-addr` | `:8080` | listen address |
| `-store` | (empty) | SQLite database path; empty = in-memory only |
| `-version-interval` | `0` (off) | capture a version of each changed document at this interval; requires `-store` |
| `-keep-versions` | `10` | keep at most N versions per document (0 = keep all) |
| `-path-prefix` | `/` | URL path the documents live under; the rest of the path is the document name |
| `-read-limit` | `0` (4 MiB) | max WebSocket frame the server reads per client; raise for large documents, `-1` = unlimited |
| `-awareness-timeout` | `0` (30s) | evict a presence entry after it is silent this long |
| `-max-awareness-clients` | `0` (4096) | cap distinct presence clients per room; `-1` = unlimited |
| `-max-conns-per-doc` | `0` (4096) | cap simultaneous WebSocket connections per document; `-1` = unlimited. A previously uncapped deployment serving a room with more than 4096 concurrent connections must set `-1` to preserve that behavior after upgrade. |
| `-max-docs` | `0` (unlimited) | cap distinct documents held in memory at once; guards against a peer opening many fabricated docNames. Defaults to unlimited because a global cap can break a large multi-tenant server. A connection that would exceed it is refused with WebSocket code 1013 (try again later); already-resident rooms are always admitted. |
| `-max-conns` | `0` (unlimited) | cap the total simultaneous WebSocket connections across all documents; a server-wide ceiling complementing `-max-conns-per-doc`. Defaults to unlimited (a total-connection count scales with deployment size). A connection that would exceed it is refused with WebSocket code 1013 (try again later) before its room is created; established connections keep working. |
| `-write-timeout` | `0` (10s) | close a client once a write to it has waited this long (a client that does not read sees the connection drop, not a close code), or once a message has waited this long in its send queue (1013, "send queue stalled"): the client has fallen behind its room and resyncs when it reconnects |
| `-max-send-queue` | `0` (4096) | close a client when another of its room's messages arrives while this many wait in its queue (the one being written is not counted): with 1013, "send queue full", if it still reads, otherwise by `-write-timeout`; `-1` = unlimited. Each client has its own queue, so a slow one never delays the others |
| `-max-send-queue-bytes` | `0` (16 MiB, or 4x `-read-limit` when larger) | the same bound in bytes; a backlog threshold, not a message size limit; `-1` = unlimited |

## Document history (versioning)

With `-version-interval 10m`, every document that changed in the last
ten minutes is captured as a named version, independent of the live
update log: log compaction never touches history, and pruning history
never touches live state. Restore is atomic.

Versions are managed programmatically via the
[`persist`](https://pkg.go.dev/github.com/Deln0r/ygo/persist) package:

```go
store, _ := sqlite.Open("data.db")
infos, _ := store.ListVersions(ctx, "my-document")          // metadata
docAt, _ := persist.LoadVersion(ctx, store, "my-document",  // inspect
    infos[0].ID, ygo.Options{})
_ = store.RestoreVersion(ctx, "my-document", infos[0].ID)   // roll back
```

## Protocol coverage

yserve speaks the `y-websocket` protocol: Sync (Step1 / Step2 / Update)
and Awareness, plus QueryAwareness. It also implements Auth, Stateless,
BroadcastStateless, Close and SyncStatus with Hocuspocus's message
numbers, on the same frames and without Hocuspocus's document-name
prefix, so no stock client sends them. `server.Options.OnAuthenticate`
and `OnStateless` hook those messages. To authenticate a stock client,
check the upgrade request in `OnConnect`
(see [embedding](#embedding-in-your-own-go-backend)).

The CRDT engine underneath is byte-for-byte wire-compatible with
`yjs@13.6.33`, apart from a few Any values, verified by the cross-language fixture suite in CI (the count lives in README.md, where a CI check keeps it honest)
(see the [main README](../README.md)).

## Migrating from y-sweet

[y-sweet](https://github.com/jamsocket/y-sweet) self-hosters: note
that y-sweet speaks its own protocol and token scheme, so the move is
a client-side change from `@y-sweet/client` to `y-websocket`. Server-side,
documents transfer as regular Yjs updates: export each doc's state
(`Y.encodeStateAsUpdate` through any y-sweet client) and replay it
into yserve, or write the update blobs straight into the SQLite store
via the `persist` API.

## Coming from Hocuspocus

Not a drop-in today: a Hocuspocus deployment has to move its clients to
`y-websocket` first. Server-side extensions differ as well: Hocuspocus
extensions are JavaScript, and yserve's equivalents are Go callbacks
plus the `persist.Store` interface for custom storage backends.

## Embedding in your own Go backend

yserve is a thin flag-parsing wrapper around
[`server`](https://pkg.go.dev/github.com/Deln0r/ygo/server), which
mounts as a standard `http.Handler`:

```go
srv := server.New(server.Options{
    Store:           store,             // any persist.Store
    OriginPatterns:  []string{"app.example.com"},
    OnConnect:       checkRequest,      // reject at the upgrade
    VersionInterval: 10 * time.Minute,  // built-in history
    KeepVersions:    10,
})
mux.Handle("/collab/", srv.Handler())
```

This is the deployment shape a sidecar process cannot give you: the
sync server lives inside your existing Go monolith, sharing its
lifecycle, metrics, and auth.

### Read-only connections

`Options.ReadOnly` grants viewer access from the upgrade request. When
it returns true for a connection, the server drops that connection's
document updates (they reach neither the document nor other peers),
while still delivering updates to it and still accepting its awareness
(so a viewer sees edits and shows a cursor):

```go
srv := server.New(server.Options{
    ReadOnly: func(docName string, r *http.Request) bool {
        return r.URL.Query().Get("mode") == "view" // or a JWT claim, header, ...
    },
})
```

It is library-only by design: a per-request predicate cannot be a CLI
flag, so yserve exposes no equivalent option. A rejected edit is dropped
silently, so it remains in the client's local document; a read-only
client should disable editing in its UI (and reload or re-sync if it
edited anyway) to avoid diverging from the server. Server-side
enforcement is the backstop, not the client's only guard.

## Operational notes

- **Single-writer storage.** SQLite serializes writes at the file
  lock; run one yserve per database file. Horizontal multi-node relay
  is not available in this binary: embed the `server` package with a
  backplane, or put a sticky-session load balancer in front and shard
  documents by path.
- **Graceful shutdown.** SIGINT/SIGTERM flushes every open document's
  update log into a single compacted snapshot before exit.
- **`ygo-server`** is the deprecated former name of this binary; it
  still builds and runs, but new flags land in yserve only.
