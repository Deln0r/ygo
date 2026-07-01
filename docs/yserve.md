# yserve — self-hosted Yjs server in a single binary

yserve is a self-hosted Yjs sync backend that replaces a
[Hocuspocus](https://github.com/ueberdosis/hocuspocus) deployment with
one static binary: same wire protocol, so your existing
`@hocuspocus/provider` and `y-websocket` clients connect unchanged.
SQLite persistence and periodic document versioning are built in.

**No Node. No Redis. No CGO.** Pure Go from the WebSocket accept loop
down to the SQLite pages (via `modernc.org/sqlite`), which is why the
Docker image is `FROM scratch` and the binary cross-compiles to any
Go target.

## Quick start

```bash
go install github.com/Deln0r/ygo/cmd/yserve@latest
yserve -addr :8080 -store data.db
```

or with Docker:

```bash
docker build -t yserve https://github.com/Deln0r/ygo.git
docker run -p 8080:8080 -v yserve-data:/data yserve
```

Point any Yjs client at it — nothing about the client changes:

```js
import * as Y from 'yjs'
import { HocuspocusProvider } from '@hocuspocus/provider'

const provider = new HocuspocusProvider({
  url: 'ws://localhost:8080',
  name: 'my-document',
  document: new Y.Doc(),
})
```

`y-websocket`'s `WebsocketProvider` works the same way (documents are
addressed by URL path, y-websocket convention).

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `-addr` | `:8080` | listen address |
| `-store` | (empty) | SQLite database path; empty = in-memory only |
| `-version-interval` | `0` (off) | capture a version of each changed document at this interval; requires `-store` |
| `-keep-versions` | `10` | keep at most N versions per document (0 = keep all) |
| `-read-limit` | `0` (32 KiB) | max WebSocket frame the server reads per client; raise for large documents, `-1` = unlimited |
| `-awareness-timeout` | `0` (30s) | evict a presence entry after it is silent this long |
| `-max-awareness-clients` | `0` (4096) | cap distinct presence clients per room; `-1` = unlimited |
| `-max-conns-per-doc` | `0` (4096) | cap simultaneous WebSocket connections per document; `-1` = unlimited. A previously uncapped deployment serving a room with more than 4096 concurrent connections must set `-1` to preserve that behavior after upgrade. |
| `-max-docs` | `0` (unlimited) | cap distinct documents held in memory at once; guards against a peer opening many fabricated docNames. Defaults to unlimited because a global cap can break a large multi-tenant server. A connection that would exceed it is refused with WebSocket code 1013 (try again later); already-resident rooms are always admitted. |

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

For comparison: in the Hocuspocus ecosystem, document history is a
paid Tiptap Cloud feature.

## Protocol coverage

yserve speaks the full 8-message Hocuspocus envelope: Sync (Step1 /
Step2 / Update), Awareness, QueryAwareness, Auth, Stateless,
BroadcastStateless, Close, SyncStatus. Authentication plugs in via
`server.Options.OnAuthenticate`; the stateless channel via
`OnStateless` (see [embedding](#embedding-in-your-own-go-backend)).

The CRDT engine underneath is byte-for-byte wire-compatible with
`yjs@13.6.31`, verified by 158 cross-language fixture scenarios in CI
(see the [main README](../README.md)).

## Migrating from y-sweet

[y-sweet](https://github.com/jamsocket/y-sweet) self-hosters: note
that y-sweet speaks its own protocol and token scheme, so the move is
a client-side change from `@y-sweet/client` to `@hocuspocus/provider`
or `y-websocket` (both standard Yjs ecosystem providers). Server-side,
documents transfer as regular Yjs updates: export each doc's state
(`Y.encodeStateAsUpdate` through any y-sweet client) and replay it
into yserve, or write the update blobs straight into the SQLite store
via the `persist` API.

## Migrating from Hocuspocus

Drop-in for the wire protocol: keep your providers, change the URL.
Server-side extension hooks differ — Hocuspocus extensions are
JavaScript; yserve's equivalents are Go callbacks (`OnAuthenticate`,
`OnStateless`) plus the `persist.Store` interface for custom storage
backends.

## Embedding in your own Go backend

yserve is a thin flag-parsing wrapper around
[`server`](https://pkg.go.dev/github.com/Deln0r/ygo/server), which
mounts as a standard `http.Handler`:

```go
srv := server.New(server.Options{
    Store:           store,             // any persist.Store
    OriginPatterns:  []string{"app.example.com"},
    OnAuthenticate:  checkToken,        // Hocuspocus auth envelope
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
  is not built in (if you need cross-node fan-out today, put a
  sticky-session load balancer in front and shard documents by path).
- **Graceful shutdown.** SIGINT/SIGTERM flushes every open document's
  update log into a single compacted snapshot before exit.
- **`ygo-server`** is the deprecated former name of this binary; it
  still builds and runs, but new flags land in yserve only.
