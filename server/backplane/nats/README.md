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

## Stronger delivery: JetStream

`natsbp.New` uses **core NATS** (at-most-once, fire-and-forget): if the client
misses a message — notably across a NATS reconnect or server restart — that
delta is silently gone. `natsbp.NewJetStream` publishes into a persistent stream
and consumes it with a JetStream **ordered consumer** that transparently
recreates itself and resumes from the last delivered message across a reconnect
or server restart, so delivery is not silently and permanently lost on a
transient outage. It requires JetStream enabled on the NATS server:

```go
ctx := context.Background()
bp, err := natsbp.NewJetStream(ctx, nc) // ensures the backing stream exists
if err != nil { /* ... */ }
```

The same `server.Options` wiring applies; it is a drop-in replacement for `New`.
No-loss is bounded by the stream's retention (`WithJSMaxAge`, default 10m): a
reconnect or restart within that window redelivers everything published during
the outage; a longer outage prunes older deltas, which the shared Store then
backfills when the document next loads (as with the core adapter). A reset may
redeliver a message, so duplicates are possible and harmless: applying a ygo
update is idempotent and commutative, as is an awareness update
(last-writer-wins by clock).

The stream is file-backed, so it and its data survive a JetStream server
restart. Options: `WithJSPrefix` (subject prefix), `WithStreamName` (default
`YGO_BACKPLANE_<prefix>`), `WithJSMaxAge` (retention / loss-free reconnect
window, default 10m). `WithJSMaxAge` is **stream-wide** and takes effect only on
the instance that first creates the stream; later instances reuse the existing
stream and never rewrite its config. The stream captures `<prefix>.>`;
distinct-prefix deployments get distinct default stream names, and a name
collision whose subjects do not match is a fail-fast error rather than a silent
rewrite. `Close` releases this instance's consumers but leaves the stream and
the connection (both caller-owned) intact.

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
- **`New` (core NATS) is at-most-once.** A dropped message is a dropped causal
  dependency: it silently parks every later edit from that client on the
  receiving instance (the apply reports no error). The shared Store heals it
  only when the document is next (re)loaded, i.e. on eviction and reload — a
  continuously-resident (hot) document never reloads, so the Store is not an
  automatic backstop for it; and client reconnect heals only if a reconnecting
  client actually holds the missing delta (the server does not re-request gaps).
  Where silent, persistent single-instance divergence on a dropped delta is
  unacceptable, use `NewJetStream` (at-least-once) instead of `New`.
- **Presence/awareness IS carried.** Payloads are opaque to the adapter, so the
  server's presence updates relay across instances like document updates (this
  changed in ygo v1.15.0; earlier the backplane carried documents only).
