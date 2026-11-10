# yrs port notes: Sync protocol (y-protocols/sync + Hocuspocus framing)

> Sources: `y-protocols` `master`, `src/sync.js` + `PROTOCOL.md` — the inner three-message sub-protocol and state machine. `y-websocket` `v2`, `bin/utils.js` — the lighter-weight reference server (`messageSync`, `messageAwareness`, `setupWSConnection`, `messageListener`, `updateHandler`). `hocuspocus` `main`, `packages/server/src/types.ts` + `MessageReceiver.ts` — the richer Hocuspocus envelope (10-tag `MessageType` enum). Cross-reference: `@hocuspocus/provider` `HocuspocusProviderWebsocket.ts` for the client-side dispatcher.

The Yjs network layer is a two-tier stack. The **inner** tier is `y-protocols/sync` — three varuint-discriminated messages (`SyncStep1`, `SyncStep2`, `Update`) implementing a "tell me what you have, here is what you are missing, here is something new" handshake on top of the V1 update format. The **outer** tier is a transport envelope that multiplexes Sync alongside Awareness (and, for Hocuspocus, Auth / QueryAwareness / Stateless / SyncStatus / Ping). Two outer envelopes exist in the wild: the minimal y-websocket envelope (`0=Sync`, `1=Awareness`, `2=Auth`) used by the reference server most JS adopters deploy, and the richer Hocuspocus envelope which is a strict superset. Both wrap the same sync.js sub-messages identically — interop between a y-websocket client and a Hocuspocus server works for the Sync+Awareness subset out of the box.

---

## Layer 1: y-protocols/sync.js

### Message-type constants (`sync.js:37-39`)

```js
export const messageYjsSyncStep1 = 0
export const messageYjsSyncStep2 = 1
export const messageYjsUpdate    = 2
```

These are written/read as `varuint` discriminators at the start of every sync sub-message.

### Wire layout — byte-level

```
SyncStep1 :=
    varuint(0)                       // messageYjsSyncStep1
  • varbuffer(encodedStateVector)    // Y.encodeStateVector(doc) — varuint(len) • bytes

SyncStep2 :=
    varuint(1)                       // messageYjsSyncStep2
  • varbuffer(encodedUpdate)         // Y.encodeStateAsUpdate(doc, remoteStateVector)

Update :=
    varuint(2)                       // messageYjsUpdate
  • varbuffer(encodedUpdate)         // a V1 update — same encoding as SyncStep2 payload
```

Citations: `sync.js:44-49` `writeSyncStep1` (writes the tag then `writeVarUint8Array(encoder, encodeStateVector(doc))`); `sync.js:54-57` `writeSyncStep2` (tag + `writeVarUint8Array(encoder, encodeStateAsUpdate(doc, encodedStateVector))`); `sync.js:85-88` `writeUpdate` (tag + `writeVarUint8Array(encoder, update)`). The `readSyncStep2` / `readUpdate` (`sync.js:62-79`, `:92-93`) are functionally identical — `readUpdate` is a direct alias to `readSyncStep2` (`sync.js:92-93`), because once decoded the payload is fed to the same `Y.applyUpdate(doc, payload, transactionOrigin)`.

The state-vector and update payloads themselves are documented in `update-v1.md` — note they are wrapped in `varbuffer` (length-prefixed byte array) at this layer, so the receiver can extract them opaquely without parsing.

### Dispatcher — `readSyncMessage` (`sync.js:98-116`)

The receiver reads the leading varuint, switches, decodes the payload, and *may* write a reply into the supplied `encoder`. `readSyncMessage` returns the message type so the outer layer can decide whether anything was written (empty encoder = no reply; y-websocket `utils.js:165-170`). Only `readSyncStep1` writes a reply — appending a `SyncStep2` against the received state vector. `readSyncStep2` and `readUpdate` apply silently.

### State machine

```
   client                                       server
   ──────                                       ──────
   open WS ─────────────────────────────────►   accept, bind to docName from URL

   SyncStep1(SV_client) ────────────────────►   apply: read SV_client
   ◄────────────────────── SyncStep2(diff)      respond with Y.encodeStateAsUpdate(doc, SV_client)
   ◄────────────────────── SyncStep1(SV_srv)    server also wants client's news
   SyncStep2(diff) ─────────────────────────►   apply, sync done

   ── steady state: bidirectional Update broadcasts ──
   local edit → Update(bytes) ──────────────►   apply, fan out to other conns
   ◄────────────────────── Update(bytes)        from another client
```

Per `PROTOCOL.md` §3.3: client-server flows have the client open with `SyncStep1`; the server replies with `SyncStep2` *and* its own `SyncStep1`. Peer-to-peer flows have both peers send `SyncStep1` on connect — symmetric. `SyncStep2` payloads may be empty when the receiver is already up-to-date — see Gotcha #2.

---

## Layer 2: Hocuspocus message envelope

`MessageType` enum (`packages/server/src/types.ts:56-67`):

| Tag | Name | Payload |
|-----|------|---------|
| `-1` | `Unknown` | sentinel |
| `0` | `Sync` | nested sync.js sub-message (varuint sub-type + varbuffer) |
| `1` | `Awareness` | `varbuffer(awarenessUpdate)` — see `awareness.md` |
| `2` | `Auth` | auth sub-protocol (token request / permission denied) |
| `3` | `QueryAwareness` | empty — request remote awareness snapshot |
| `4` | `SyncReply` | same as `Sync`, server-initiated reply (internal) |
| `5` | `Stateless` | `varstring(payload)` — opaque user channel |
| `6` | `BroadcastStateless` | `varstring(payload)` — fan-out variant |
| `7` | `CLOSE` | close reason (uppercase per upstream) |
| `8` | `SyncStatus` | one-byte synced flag |
| `9` | `Ping` | empty (one-byte frame heuristic) |
| `10` | `Pong` | empty |

### Outer envelope byte layout

```
Message       := varuint(messageType) • payload
Sync          := varuint(0) • varuint(syncSubType) • varbuffer(bytes)
Awareness     := varuint(1) • varbuffer(awarenessUpdate)
QueryAwareness:= varuint(3)                         // payload empty
```

The outer layer adds **no extra length prefix** around the nested sync message — the y-protocols layer is self-delimited via its own `varbuffer` payload, and the WS frame boundary delimits the whole envelope. `MessageReceiver.apply` (`MessageReceiver.ts:33` ff) switches on the outer varuint, delegating `Sync` to `readSyncMessage` (`:37-54`), `Awareness` to `applyAwarenessUpdate` (`:55-61`), `QueryAwareness` to `applyQueryAwarenessMessage` (`:62-66`), `Stateless`/`BroadcastStateless` to a user callback (`:67-76`). `Auth` (`:77-89`) is special — see §"Auth flow".

---

## Bare y-websocket protocol

The lightweight reference server at `y-websocket` `v2/bin/utils.js` (`messageSync = 0`, `messageAwareness = 1`, commented-out `messageAuth = 2`; `utils.js:68-70`) implements a strict subset of the Hocuspocus envelope — only tags `0` and `1` are routed (`utils.js:159-176`); auth is stubbed and `QueryAwareness` is absent. Wire layout for the two supported tags is byte-identical to Hocuspocus, so a Hocuspocus client against a y-websocket server round-trips Sync and Awareness and only fails on Auth/QueryAwareness/Stateless/SyncStatus traffic (which y-websocket servers do not emit). **A ygo server that speaks tags 0+1 interoperates with both ecosystems — the 95% case.** Implement that subset first; tags 2/3/5/6/7/8 are v0.2.

Key reference points in `utils.js`: `setupWSConnection` (`utils.js:182-184`) extracts docName as `req.url.slice(1).split('?')[0]` — one WS = one doc. `messageListener` (`utils.js:159-181`) is the outer varuint dispatcher. Server-side initial syncStep1 push (`utils.js:199-203`) writes `varuint(messageSync) + writeSyncStep1(encoder, doc)` immediately on connect; the client also opens with its own `SyncStep1` (`HocuspocusProviderWebsocket.ts:218-226`) — they cross on the wire. `updateHandler` (`utils.js:77-83`) broadcasts local document updates to **every** connection including the originator (no skip-self) — safe because Yjs updates are idempotent. Hocuspocus follows the same pattern. See Gotcha #6.

---

## Go translation choices

Recommended layout. `internal/sync/protocol.go` holds typed `MessageType uint8` and `SyncSubType uint8` constants (Sync=0, Awareness=1, Auth=2, QueryAwareness=3, SyncReply=4, Stateless=5, BroadcastStateless=6, Close=7, SyncStatus=8, Ping=9, Pong=10; SyncStep1=0, SyncStep2=1, SyncUpdate=2). `internal/sync/framing.go` exposes pure encode/decode primitives — `EncodeSyncStep1(sv []byte)`, `EncodeSyncStep2(update []byte)`, `EncodeSyncUpdate(update []byte)`, `EncodeAwareness(update []byte)`, `EncodeQueryAwareness()`, `DecodeEnvelope(b []byte) (Frame, error)` — reusing `internal/lib0` (`AppendVarUint`, `AppendVarBytes`, `ReadVarUint`, `ReadVarBytes`). `internal/sync/handler.go` owns the state machine: one `ConnState` per WS, with hooks into `internal/awareness.Awareness` and `internal/encoding.Pending` for updates that arrive before the initial `SyncStep2` lands.

**WebSocket library: `github.com/coder/websocket`** (formerly `nhooyr.io/websocket`) over `github.com/gorilla/websocket`. Reasons: (1) `context.Context`-native API composes with our per-request HTTP context for cancellation; (2) message-oriented `Read(ctx) (MessageType, []byte, error)` matches our framing exactly; (3) pure-Go, zero CGo; (4) gorilla has been in maintenance mode since 2022 and lacks context cancellation, forcing per-read goroutine wrappers to enforce timeouts. Mitigate gorilla's install-base advantage by exposing a `Conn` interface in `server/` so users can swap in a gorilla backend if needed.

**Public API.** `server.New(opts Options) *Server` where `Options{ Store persist.Store; Awareness bool; OnUpdate func(docName string, update []byte); OnAuthenticate func(ctx context.Context, docName, token string) error }`. `*Server` exposes `Handler() http.Handler` so users mount on their own mux at any path prefix; docName is the trailing path segment, matching y-websocket's `req.url.slice(1).split('?')[0]` rule. `cmd/ygo-server/main.go` is a 30-line `http.ListenAndServe` wrapper. Shape mirrors `net/http` ergonomics rather than imposing a runtime — every ygo user already has an HTTP server.

---

## Auth flow

> Corrected 10 Nov 2026, from captures of the real providers and servers: the Auth sub-types are Token 0, PermissionDenied 1, Authenticated 2 (`@hocuspocus/common` `auth.ts`), every frame starts with `varString(documentName)`, an accepted token gets Authenticated with a scope, and a refused one gets PermissionDenied with a reason while the socket stays open. The paragraphs below are the original notes. See `docs/tech-debt.md`, "Hocuspocus framing".

Hocuspocus's auth path: client sends `MessageAuth` (tag `2`) with payload `varuint(authSubType) • varstring(token)` (`AuthMessageType.Token`; `MessageReceiver.ts:77-89`). Server invokes `onAuthenticate(token)`; on success no reply is needed and Sync proceeds; on failure the server sends a Close (tag `7`) with a "permission denied" reason and terminates the WS with code `4401`.

In Go: `Options.OnAuthenticate func(ctx, docName, token string) error` — `nil` accepts, error sends `MessageClose`. **Recommendation: defer to v0.2.** Bare y-websocket does not support auth at all (commented out at `utils.js:70`), so a y-websocket-compatible server with zero auth covers the majority of adopters. The full `AuthMessageType` sub-tag protocol and permission-denied close-code dance is a non-trivial v0.1 surface.

---

## Awareness integration

Awareness travels on the same WS connection as Sync, multiplexed via outer tag `1`. Our `internal/awareness` package already owns encode/decode of the payload (`awareness.md` §3); the sync layer only adds the outer `varuint(1)` tag and the `varbuffer` wrapper.

Wire flow (`utils.js:107-122`, `:172-174`, `:195`): server registers an `awarenessChangeHandler` against `doc.awareness` that fans out a tag-`1` envelope to every connection whenever state changes; on receive of a tag-`1` envelope, calls `applyAwarenessUpdate(doc.awareness, payload, conn)` with `conn` as the origin (so the change handler can update the per-connection `Set<clientID>` ownership map at `utils.js:109-115`); on disconnect, calls `removeAwarenessStates(doc.awareness, controlledIds, null)`, which fires removal events to all peers.

Go: `server/conn.go` holds `controlledClients map[uint64]struct{}` per connection. On `MessageAwareness`, call `awareness.Apply(payload, conn)` and reconcile from the returned `Summary{Added, Removed}`. On WS close, iterate `controlledClients` and call `awareness.RemoveState(clientID)` for each — the resulting removal events drive the broadcast handler.

---

## Gotchas — implementer must not miss

1. **QueryAwareness handshake.** Hocuspocus clients may send a `varuint(3)` envelope (empty payload) at any time to request the current full awareness snapshot (`HocuspocusProviderWebsocket.ts:65-72`, `MessageReceiver.ts:62-66`). Server must reply with a tag-`1` envelope containing `encodeAwarenessUpdate(awareness, Array.from(awareness.states.keys()))`. y-websocket servers do NOT support this — so a Hocuspocus client against a y-websocket server silently misses the bulk snapshot and recovers only on next incremental change. Implement in v0.1 even if you skip Auth.

2. **SyncStep2 may be empty.** If the receiver has nothing the sender is missing, `Y.encodeStateAsUpdate(doc, remoteSV)` returns a well-formed V1 update with zero blocks and empty delete set — wire bytes `varuint(0) • varuint(0)`, wrapped in the outer `varbuffer`. Do not treat empty payloads as errors; `applyUpdate` on an empty update is a no-op.

3. **Updates may arrive before SyncStep2 lands.** Once the WS is open both peers are free to broadcast `Update` messages; an update with `clock > localStateVector[clientID]` cannot be integrated until intermediates arrive. Route every incoming `MessageSync` with `SyncUpdate` (and the V1 update inside `SyncStep2`) through `encoding.Pending`, not directly into the doc.

4. **WebSocket framing length-prefixing is independent.** The WS frame already carries its own payload length — the y-protocols layer does not re-prefix the whole envelope. It DOES prefix the inner payload (state vector or update bytes) via `varbuffer`, so `readSyncMessage` knows where it ends without consulting the WS frame boundary. Do not add a redundant outer length tag.

5. **One WebSocket = one document.** docName is extracted from the URL path at connect time (`utils.js:182-184`). All Sync and Awareness traffic on that connection refers to that one doc. Multiplexing multiple docs onto one WS is not part of either protocol — clients open a separate WS per doc.

6. **Skip-self is NOT enforced at the network layer by the reference servers.** Both `utils.js:77-83` and Hocuspocus's `Document.broadcast` fan out to every connection including the originator. Safe because Yjs updates are idempotent. Do not add a skip-self check unless you have profiled a real win; deviating risks subtle bugs where the originator's local state is racy and the echo would actually correct it.

7. **Hocuspocus may emit V2 updates we do not support.** Some Hocuspocus extensions call `Y.encodeStateAsUpdateV2`. ygo implements only V1. Reject V2 frames cleanly with a clear error rather than crashing; V2 codec is a v0.3 workstream.

8. **Connection-counted doc lifecycle.** A doc's in-memory state is freed only when its last connection closes (`utils.js:196-202`). Servers must reference-count conns per doc and persist on the final disconnect; failure leaks `*Doc` instances.

9. **Ping/Pong are application-layer, not WS-layer.** Hocuspocus encodes `Ping`=`9` / `Pong`=`10` inside the outer envelope (`types.ts:65-66`); the provider treats any one-byte frame as Ping. Servers should respond with a one-byte frame `varuint(10)`. y-websocket uses WS-level ping/pong only (`utils.js:223`) — harmless asymmetry because WS keepalive covers both.

10. **Order across Sync and Awareness matters at connect.** Most clients send `varuint(0)+SyncStep1` first then `varuint(1)+initialAwareness` very shortly after. A naive server that drops the awareness message because the doc is not yet "ready" will desync presence. Process both tags from the first message; do not gate awareness behind sync completion.
