# yserve — self-hosted Yjs server in a single binary

yserve is a self-hosted Yjs sync backend in one binary. It speaks the
`y-websocket` protocol, so `y-websocket` clients connect to it unchanged
(checked with y-websocket 2.1 and 3.1). SQLite persistence and periodic
document versioning are built in.

Hocuspocus server support shipped in 1.37.0. Providers 2.15.3,
3.4.4 and 4.8.0 connect to the base URL, with the document name in their
configuration. See [Coming from Hocuspocus](#coming-from-hocuspocus).

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

- **Hocuspocus.** Token refresh on an established key, server-initiated
  token requests and Redis cluster opcodes are not implemented. Close and
  reopen a key to change its token or scope. The Go backplane interface
  provides document and presence relaying between ygo instances.
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

yserve serves y-websocket Sync, Awareness and QueryAwareness, and
Hocuspocus document-prefixed Sync, Awareness, Auth, Stateless, Close and
SyncStatus frames. Both protocols can edit the same document. Hocuspocus
keys are authenticated before the document is loaded; an unknown or denied
key cannot receive the room's broadcasts. For y-websocket clients, check
the upgrade request with `OnConnect`.

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

Point the provider at the base URL, without appending the document name:

```js
import * as Y from 'yjs'
import { HocuspocusProvider } from '@hocuspocus/provider'

const provider = new HocuspocusProvider({
  url: 'ws://localhost:8080',
  name: 'my-document',
  document: new Y.Doc(),
})
```

With `-path-prefix /ws/`, use `ws://localhost:8080/ws`. Providers strip
trailing slashes. When embedding the server, mount
`srv.HocuspocusHandler()` on an exact path such as `/collab`, and
`srv.Handler()` on `/collab/` for y-websocket documents.

Use `Options.OnAuthenticateRequest(r, docName, token)` to check a token
with request headers or cookies and return its read-only permission.
`OnAuthenticate(docName, token)` is the fallback; `ReadOnly` can impose
an additional restriction. These hooks run once for each newly admitted
key, as do `OnConnect` and `OnDisconnect`. A denied token gets
PermissionDenied, drops the key's pending frames, and leaves the socket
open for another attempt. With neither auth hook, a key can sync without
a token, including provider 2's no-token mode.

Multiple keys share one socket and send queue. `MaxConns` counts sockets,
including those with no admitted keys; `MaxConnsPerDoc` and
`Stats.Connections` count document keys. Pre-authentication buffering is
limited to 1000 messages, 5 MiB and 100 pending keys across the socket;
exceeding a limit closes it with 4205. `AuthTimeout` gives the socket
60 seconds to admit its first key, regardless of inbound traffic.
`IdleTimeout` closes a silent socket after 60 seconds with 4408. Both
are configurable through `server.Options`.

`Server.SendStateless(docName, payload)` broadcasts to the document's
Hocuspocus peers. A provider's CLOSE removes only its key; a socket error
or send-queue overflow disconnects all its keys. Provider 4's
`sessionAwareness` keys share document content while retaining their
individual routing keys and permissions.

Hocuspocus extensions are JavaScript; port them to Go callbacks and the
`persist.Store` interface when moving the server.

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

### Changing documents from the server

Since 1.52.0 server code can change a document the way a client does. The
write is applied to the server's copy, queued to every connection of the
document (y-websocket and Hocuspocus, read-only viewers included), stored,
published on the Backplane and reported to `OnChange`:

```go
// One transaction; roots through the *In accessors.
update, err := srv.Apply(ctx, "doc-42", func(txn *ygo.TransactionMut) error {
    ygo.MapIn(txn, "meta").Set(txn, "status", "published")
    return nil
})

// An update from elsewhere (a job, another system): checked, applied, sent.
err = srv.ApplyUpdate(ctx, "doc-42", update)

// Hold a document open across several writes and reads.
d, err := srv.OpenDocument(ctx, "doc-42")
state, _ := d.State() // the server's copy as one V1 update
d.Close()

// Take a document out of the server: refused while clients are connected
// unless forced, which closes with 1001 its y-websocket connections and the
// Hocuspocus sockets holding it (their other documents reconnect). It waits
// for every open Document on it to be closed, so do not call it while
// holding one.
err = srv.CloseDocument(ctx, "doc-42", true)
```

- A document no client holds is loaded for the call as a connection would load
  it (Store, then `OnLoadDocument`, counted against `MaxDocs`), then evicted or
  parked as after a last disconnect. On a server without a Store, a document a
  server write changed stays resident until `CloseDocument` or `Close`, so the
  write is not lost.
- `ApplyUpdate` applies the update to the server's document as well as sending
  it, so a client that connects later gets it in its sync.
- The transaction of each write has a new `*server.ServerOrigin` as its origin.
  `fn` runs under the document's write lock: it must not call the server for the
  same document or wait on its clients. Edits made before `fn` returns an error
  or panics are committed and sent all the same.
- `Options.OnServerWrite` sees each write before it happens and can refuse it
  (`ErrWriteRefused`).
- `Close` waits, within its context, for open Documents and writes in progress,
  and the document API (Documents opened before it included) returns
  `ErrServerClosed` after it. It also drops documents retained without a Store.
- A forced `CloseDocument` closes a whole Hocuspocus socket, because only the
  socket's own read loop may release its documents (a frame it is handling
  could otherwise land on the closed copy); the provider reconnects and
  re-attaches the socket's other documents. Closing one document with a CLOSE
  message, as Hocuspocus's `closeConnections` does, is not offered: provider
  2.15 throws on it, and providers 3.4 and 4.8 stop syncing that document until
  the application detaches and re-attaches it.
- Do not call `CloseDocument` from a hook for a document the hook's own
  connection holds (any document of a Hocuspocus socket, or a y-websocket
  connection's own document in `OnConnect`): it waits for that connection to
  be released, which happens after the hook returns, so it returns only when
  its context ends. Call it on its own goroutine.
- A Store error after the write was applied and sent comes back wrapped in
  `ErrNotStored`.

### Sync over HTTP

Since 1.54.0 `HTTPSyncHandler` serves the server's documents to clients that
pull and push updates with plain requests: cron jobs, serverless functions,
clients behind proxies that drop WebSockets, scripts. It serves the same
documents as the WebSocket handlers, so an HTTP write reaches the WebSocket
clients of the document at once, and their edits are in the next pull.

```go
srv := server.New(server.Options{
    Store: store,
    // Required: the WebSocket hooks do not cover HTTP requests.
    OnHTTPRequest: func(r *http.Request, docName string, write bool) error {
        if !validToken(r.Header.Get("Authorization"), docName, write) {
            return &server.HTTPError{Status: http.StatusUnauthorized, Message: "bad token"}
        }
        return nil
    },
    DocIdleTimeout: time.Minute, // keeps polled documents resident (ETags)
})
mux.Handle("/collab/", srv.Handler())
mux.Handle("/sync/", srv.HTTPSyncHandler()) // /sync/<doc>
```

- `GET /sync/<doc>?sv=<base64 state vector>` returns the document's update
  for a client at that state vector (the whole document without `sv`), with
  its pending updates, the bytes a sync step 2 carries; `v=2` returns V2. The
  `Ygo-State-Vector` header is the server's state vector (base64) at that
  moment.
- `POST /sync/<doc>` with `Content-Type: application/octet-stream` and one V1
  update as the body applies it as a client's update: every connection gets
  it, the Store stores it, the Backplane publishes it, `OnChange` sees it; the
  transaction's origin is a `*server.HTTPOrigin`. With `?sv=`, the response
  is 200 with the document's update for that state vector after the write
  (push and pull in one request), else 204.
- Every response carries an `ETag` naming the document's revision. A GET
  with `If-None-Match` set to the last one returns 304 with no body when the
  document has not changed since. A tag belongs to one loaded copy of the
  document: without `DocIdleTimeout`, a document no client holds is loaded
  for each request and every poll is a 200.
- `sv` is base64, standard or URL-safe, padded or not.

From JavaScript, with yjs and `fetch`:

```js
const b64 = (u8) => btoa(String.fromCharCode(...u8))
const unb64 = (s) => Uint8Array.from(atob(s), (c) => c.charCodeAt(0))

const sv = b64(Y.encodeStateVector(doc))
const res = await fetch(`/sync/doc-42?sv=${encodeURIComponent(sv)}`)
Y.applyUpdate(doc, new Uint8Array(await res.arrayBuffer()))
const serverSV = unb64(res.headers.get('Ygo-State-Vector'))
await fetch('/sync/doc-42', {
  method: 'POST',
  headers: { 'Content-Type': 'application/octet-stream' },
  body: Y.encodeStateAsUpdate(doc, serverSV),
})
```

From Go, `client.HTTPSync` does both and remembers what it last sent and
received:

```go
s := &client.HTTPSync{URL: "https://example.com/sync/doc-42", Header: authHeader}
defer s.Close()
changed, err := s.Sync(ctx, d) // pull, then push only what the server lacks
```

- `OnHTTPRequest` runs for every request before its body is read or its
  document loaded; without it every request is 403. `ReadOnly` applies:
  a read-only request may GET but not POST.
- A browser's `Origin` must be the request's host or match `OriginPatterns`,
  as for WebSocket upgrades, and POST needs `application/octet-stream`,
  which a cross-site form cannot send. CORS headers are not set: wrap the
  handler in your CORS middleware to serve other origins (expose
  `Ygo-State-Vector` and `ETag` to their scripts).
- A POST body is limited by `ReadLimit` (413), each request holds one of
  `MaxConns` while it runs, and `MaxDocs` applies (503 with `Retry-After`).
  A GET of a document that is not resident and holds nothing does not keep
  it resident, so made-up names do not fill `MaxDocs`.
- Names are taken as for WebSocket requests (`DocNameFn`); HTTP refuses an
  empty name, invalid UTF-8, control characters and names over 1,024 bytes.
- POST takes V1 only; convert V2 first (`ygo.ConvertUpdateFormatV2ToV1`).
- A Store error after the update was applied and sent is a 500 that says so.
  Updates are idempotent, so a retry is safe, but `OnChange` sees it again.

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
