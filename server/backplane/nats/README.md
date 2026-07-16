# NATS backplane for ygo

A [NATS](https://nats.io)-backed `backplane.Backplane` so several ygo servers
behind a load balancer converge on the same document. It is a **separate Go
module** (`github.com/Deln0r/ygo/server/backplane/nats`) so the ygo core stays
dependency-free; import it only if you want NATS clustering.

```go
import (
    "github.com/nats-io/nats.go"
    "github.com/Deln0r/ygo/server"
    natsbp "github.com/Deln0r/ygo/server/backplane/nats"
)

nc, err := nats.Connect("nats://nats.internal:4222")
if err != nil { /* ... */ }
defer nc.Close() // you own the connection

bp, err := natsbp.New(nc) // or natsbp.New(nc, natsbp.WithPrefix("myapp"))
if err != nil { /* ... */ }

srv := server.New(server.Options{
    Store:     sharedStore, // REQUIRED for multi-instance (see below)
    Backplane: bp,
})
// srv.Close closes bp (releases its subscriptions) but not nc.
```

Each server instance builds its own `natsbp.New(nc)` (a fresh instance
identity), and they must all use the same subject prefix.

## What it does

- Publishes each applied document update on `<prefix>.<docName>` and subscribes
  to foreign updates for every resident document, applying them to the
  in-memory copy and re-broadcasting to local clients. Self-publishes are
  filtered by an instance-origin header, so there is no echo.
- `docName` is base64url-encoded into the subject, so names containing `.`,
  spaces, or the `*`/`>` wildcards cannot break routing or cross documents.
- `Subscribe` flushes before returning, so the subscription is registered on
  the client's own NATS server before the caller proceeds — the ordering the
  server's subscribe-before-load relies on. On a multi-server NATS cluster,
  interest still propagates across servers asynchronously, so a publish from
  another node during that short propagation window can be missed (the shared
  Store is the backstop for it, as for any dropped message; see the limit
  below).

## Requirements and limits

- **A shared Store is required.** Foreign updates are applied in memory only,
  never re-persisted, so every instance sharing a document must point at the
  same Store. The backplane carries live deltas; the Store is the source of
  truth an instance loads from when a document becomes resident. See the
  `server.Options.Backplane` docs.
- **Delivery is core-NATS at-most-once.** A dropped message is a dropped
  causal dependency: it silently parks every later edit from that client on
  the receiving instance (the apply reports no error). The shared Store heals
  it only when the document is next (re)loaded, i.e. on eviction and reload —
  a continuously-resident (hot) document never reloads, so the Store is not an
  automatic backstop for it; and client reconnect heals only if a reconnecting
  client actually holds the missing delta (the server does not re-request
  gaps). Where silent, persistent single-instance divergence on a dropped
  delta is unacceptable, use a JetStream-backed backplane (the future
  direction) rather than core NATS.
- **Presence/awareness is not carried** over the backplane (per-instance only),
  matching the core backplane.
