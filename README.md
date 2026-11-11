# Ygo

[![Listed in the Yjs docs](https://img.shields.io/badge/Yjs%20docs-listed%20port-7c3aed.svg)](https://docs.yjs.dev/ecosystem/ports-to-other-languages)
[![CI](https://github.com/Deln0r/ygo/actions/workflows/test.yml/badge.svg)](https://github.com/Deln0r/ygo/actions/workflows/test.yml)
[![Coverage](https://coveralls.io/repos/github/Deln0r/ygo/badge.svg?branch=main)](https://coveralls.io/github/Deln0r/ygo?branch=main)
[![Go Reference](https://pkg.go.dev/badge/github.com/Deln0r/ygo.svg)](https://pkg.go.dev/github.com/Deln0r/ygo)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![Go Version](https://img.shields.io/badge/go-1.26%2B-00ADD8.svg)](go.mod)
[![Yjs Protocol](https://img.shields.io/badge/Yjs%20protocol-V1%20%2B%20V2-7c3aed.svg)](https://github.com/yjs/yjs)
[![Live Demo](https://img.shields.io/badge/live%20demo-ygo.deln0r.com-22c55e.svg)](https://ygo.deln0r.com)
[![Codeberg Mirror](https://img.shields.io/badge/codeberg-mirror-2185d0?logo=codeberg&logoColor=white)](https://codeberg.org/Deln0r/ygo)

Pure-Go port of [Yjs](https://github.com/yjs/yjs), the CRDT framework for collaborative applications, listed on the [ports page](https://docs.yjs.dev/ecosystem/ports-to-other-languages) of the Yjs documentation.

Ygo speaks the **Yjs V1 and V2 wire formats** byte for byte, apart from object key order, whole-number floats set from Go and a format attribute set to undefined (see [Strings and values](#strings-and-values)). The encoding is verified in both directions through **307 cross-language fixture scenarios** generated from `yjs@13.6.33`. The bundled WebSocket server speaks the `y-websocket` protocol: JavaScript clients using `y-websocket` connect to it directly, and the Go client connects to the reference `y-websocket` server. On `main` (unreleased), the server also serves `@hocuspocus/provider` 2.15, 3.4 and 4.8, and the Go client connects to Hocuspocus servers of those versions (see [Hocuspocus setup](docs/yserve.md#coming-from-hocuspocus)). No CGO; `gomobile bind` produces an Android AAR (built in CI) and an iOS xcframework (built by hand).

## Highlights

- **Byte-level wire compatibility, verified in both directions.** 307 cross-language fixture scenarios (generated from `yjs@13.6.33`) cover the V1 and V2 update formats, snapshots, subdocuments, undo, observers, relative positions, GC, awareness, the sync protocol, and the Any values JavaScript has and Go does not (undefined, BigInt, -0), JS to Go and Go to JS, plus 56 lib0 primitive vectors. The suite runs in CI on every push to `main` and every pull request, so a regression in either direction fails the build. The exceptions are the Any values listed under [Strings and values](#strings-and-values).
- **Pure Go, no CGO.** The CRDT core, the sync client and the server compile with `CGO_ENABLED=0` for all 47 targets in `go tool dist list`, `js/wasm` and `wasip1/wasm` included. The SQLite-backed store in [`persist/sqlite`](persist/sqlite), and with it `yserve`, builds only where `modernc.org/sqlite` does: not on wasm, AIX, DragonFly, illumos, Plan 9, Solaris, MIPS or big-endian ppc64 Linux, NetBSD other than amd64, or OpenBSD other than amd64 and arm64. Elsewhere, supply your own [`persist.Store`](persist) (it is a seven-method interface). `gomobile bind` produces an Android AAR (built in CI on every push to `main` and every pull request) and an iOS xcframework (built by hand on 2026-11-10) carrying a full mobile SDK: editable Text / Map, undo, cursors, and a built-in background sync client (WebSocket + reconnect), so a Swift / Kotlin app only renders UI. No V8, no embedded JavaScript engine, no Rust FFI bridge.
- **Embeddable sync client, offline-first.** The [`client`](client) package is a Go-native `y-websocket` provider, and with `Options.Hocuspocus` a Hocuspocus one: handshake, incremental updates, awareness, reconnect with backoff, and a liveness check that redials a connection which went silent. With a `LocalStore` it persists the document to disk (pure-Go SQLite), loads it before any network so the app works offline, and syncs edits made offline up on reconnect. The building block for bots, CLI tools, server-side agents, and the mobile SDK (`EnableOfflineStore`).
- **The Yjs shared types.** Map, Array, Text (rich-text formatting, Quill deltas, embeds), XML types, Awareness, UndoManager, Snapshots / time-travel, and Subdocuments. Not implemented: the legacy `XmlHook`, observers on `XmlFragment` and `XmlElement`, and `Text.ObserveDeep`.
- **Change observers.** `Map.Observe` / `Array.Observe` / `Text.Observe` deliver Quill-style deltas of exactly what changed; `ObserveDeep` bubbles events from nested types with their path. Semantic parity with yjs's YMapEvent / YArrayEvent / YTextEvent. On mobile, `ObserveChanges` hands a native editor the delta as JSON.
- **One update per transaction.** `Doc.OnUpdate` / `OnUpdateV2` hand you each committed transaction as an update holding just its changes, the bytes yjs emits on `doc.on('update')` (known exceptions in the status table), with the transaction's origin; an append costs under 30 bytes however long the history. `ApplyUpdateWithOrigin` tags a peer's update so an echo guard and the UndoManager can tell it apart, and `EncodeDiff` catches a peer up from its own clock.
- **Compact encoding.** Commit-time block squash collapses per-character edits into single items (about 1 byte per character in V1), and garbage collection frees deleted content at commit. On a 260k-edit real-world trace a document with a random client ID takes 311 KB in V1 and 160 KB in V2, the same as yjs: both draw 32-bit client IDs. With the same client ID, ygo and yjs produce documents of identical size in both formats.
- **Forward-looking wire handling.** ygo handles 53-bit client IDs throughout (byte-verified above 2^32), the confirmed wire-level change in the `yjs@14` release candidate, and decodes Skip structs in the update stream as no-op gaps (part of the wire format since yjs v13.5). Full attribution / IdMap support waits for the v14 format to stabilize.
- **Ready-to-run server: [yserve](docs/yserve.md).** A self-hosted Yjs server in one binary, with no Node and no Redis. `y-websocket` clients connect unchanged (CI runs y-websocket 2.1 and 3.1 against it), and SQLite persistence and periodic document versioning are built in. The document is the URL path, slashes included; behind a proxy that keeps a path prefix, pass it as `-path-prefix`. It also embeds as a plain `http.Handler` inside an existing Go backend.
- **Scales horizontally, as a library.** Several instances of the [`server`](server) package behind a load balancer converge on the same document through a pluggable [backplane](server/backplane): edits and live cursors both cross instances, so two users on different servers collaborate as if on one. An in-process hub ships in the core; an opt-in [NATS adapter](server/backplane/nats) (a separate module, so NATS stays out of the core's dependencies) spans machines, with a JetStream mode that resumes delivery across a broker reconnect or restart. Clustered instances share one `Store` and run the same ygo version. The `yserve` binary has no backplane option.
- **Federated transport over Matrix.** An opt-in [Matrix transport](integration/matrix) (a separate module, so mautrix stays out of a build that only wants the CRDT) turns an ordinary unencrypted Matrix room into the sync channel: peers publish updates as `dev.ygo.update` room events and merge what they read back, with no ygo server involved. A room is an append-only log with no ordering or exactly-once guarantee, which is exactly what Yjs updates tolerate. Room content is treated as untrusted — validated on both sides, decoded one event at a time so one bad event cannot break a whole page, encrypted rooms refused rather than silently written in the clear — though, as the module README spells out, Yjs updates do not authenticate deletes, so room membership is write access to the document. Exercised in CI against a real Dendrite on every push to `main` and every pull request. Its limits are documented rather than papered over.
- **EU-sovereign mirror** on [codeberg.org/Deln0r/ygo](https://codeberg.org/Deln0r/ygo), auto-synced from GitHub on every push for adopters who prefer or require EU-hosted code infrastructure.

**Live demo:** open [ygo.deln0r.com](https://ygo.deln0r.com) in two browser tabs and start typing. The page runs a stock `y-websocket` client in JavaScript against a pure-Go server.

## Quick start

```go
package main

import (
    "fmt"

    "github.com/Deln0r/ygo"
)

func main() {
    src := ygo.NewDoc()
    m := ygo.NewMap(src, "settings")

    txn := src.WriteTxn()
    m.Set(txn, "theme", "dark")
    m.Set(txn, "lang", "go")
    txn.Commit()

    // Encode the source doc's full state as wire bytes.
    update := ygo.EncodeStateAsUpdate(src)

    // Apply to a fresh peer doc — same bytes JS Yjs's Y.applyUpdate consumes.
    dst := ygo.NewDoc()
    if err := ygo.ApplyUpdate(dst, update); err != nil {
        panic(err)
    }

    dstMap := ygo.NewMap(dst, "settings")
    fmt.Println(dstMap.Get("theme")) // dark
}
```

For a collaborative server backend, see [yserve](docs/yserve.md), a stand-alone WebSocket server for `y-websocket` clients with SQLite persistence and document versioning, in one binary:

```bash
# CGO_ENABLED=0 makes the binary fully static on Linux
CGO_ENABLED=0 go install github.com/Deln0r/ygo/cmd/yserve@latest
yserve -addr :1234 -store data.db -version-interval 10m
```

### Sending each change

`Doc.OnUpdate` hands you every committed transaction as a V1 update holding exactly that transaction's changes, the bytes of yjs's `doc.on('update')`, except in the pinned case listed in the status table below and for the Any values listed under [Strings and values](#strings-and-values). Forward them to peers or append them to a log; an append costs a few bytes however long the document's history is. Apply what peers send with an origin, so the handler can tell their updates from yours:

```go
type peer struct{ id string }
from := &peer{id: "ws-1"}

d.OnUpdate(func(update []byte, origin any) {
    if origin == from {
        return // came from that peer, do not send it back
    }
    broadcast(update)
})

err := ygo.ApplyUpdateWithOrigin(d, received, from)
```

`OnUpdateV2` gives the V2 bytes, and `ApplyUpdateV2WithOrigin` applies V2 bytes with an origin. Handlers run at the end of the commit with the document lock held, in commit order; they must not open a transaction on the same document. To catch up a peer that has been away, send `ygo.EncodeDiff(d, peerStateVector)`: it starts exactly at the peer's clock, as `Y.encodeStateAsUpdate(doc, sv)` does. Unlike yjs it does not append updates still waiting in the pending buffer for a missing dependency.

### Undo / Redo

Wrap any shared types in an `UndoManager` to get scoped, grouped Undo / Redo:

```go
d := ygo.NewDoc()
m := ygo.NewMap(d, "settings")

um := ygo.NewUndoManager(d, m) // watch m; defaults to local edits, 500ms grouping
defer um.Close()

txn := d.WriteTxn()
m.Set(txn, "theme", "dark")
txn.Commit()

um.Undo() // m no longer has "theme"
um.Redo() // "theme" == "dark" again
```

Only edits under the watched types made by a tracked origin are captured. The default tracks the nil origin, which local edits carry, but so does plain `ApplyUpdate`, exactly as in yjs: apply remote updates with `ygo.ApplyUpdateWithOrigin(d, update, origin)` to keep them out of the history, or track your own origin only (`um.AddTrackedOrigin(mine)`, `um.RemoveTrackedOrigin(nil)`). Rapid edits inside the capture-timeout window collapse into one undo step; call `um.StopCapturing()` to force a boundary. The semantics match `yjs@13.6.33`'s `UndoManager`, checked by cross-language conformance fixtures.

### Snapshots / time-travel

Capture a point in a document's history and reconstruct it later. The source doc must have GC disabled so deleted content is retained:

```go
d := ygo.NewDocWithOptions(ygo.Options{DisableGC: true})
txt := ygo.NewText(d, "t")

txn := d.WriteTxn()
txt.Insert(txn, 0, "world!")
txn.Commit()

snap := ygo.CreateSnapshot(d)         // mark this moment
saved := ygo.EncodeSnapshot(snap)     // persist it (byte-compatible with Y.encodeSnapshot)

txn = d.WriteTxn()
txt.Insert(txn, 0, "hello ")          // doc moves on
txn.Commit()

restored, _ := ygo.RestoreSnapshot(d, snap) // reconstruct the marked state
ygo.NewText(restored, "t").String()         // "world!"
```

The snapshot wire format (`EncodeSnapshot` / `DecodeSnapshot`) is byte-compatible with `yjs@13.6.33`'s `Y.encodeSnapshot`, verified by cross-language fixtures including multi-client delete-set ordering. `RestoreSnapshot` mirrors `Y.createDocFromSnapshot`.

### Subdocuments

Nest a `Y.Doc` inside a Map. The parent stores a reference (the subdoc's GUID); the subdocument's own content syncs as a separate update stream:

```go
d := ygo.NewDoc()
m := ygo.NewMap(d, "m")

txn := d.WriteTxn()
sub := m.SetDoc(txn, "child") // nest a new subdocument
txn.Commit()

// after syncing the parent to another replica:
got, ok := m.GetDoc(d, "child") // got.GUID() == sub.GUID()
```

The `ContentDoc` wire format (GUID + options) is byte-compatible with `yjs@13.6.33`, verified by cross-language fixtures. Lifecycle events are observable via `d.OnSubdocs` (added / removed / loaded GUIDs per transaction); `SetDocWithOptions(..., autoLoad)` and `subdoc.Load()` drive the loaded set, so a sync provider knows which nested documents to fetch. `Load` is reported with the next transaction on the parent, where yjs reports it at once.

### Strings and values

lib0 rejects strings that are not valid UTF-8, so ygo makes every string it is given valid at the call, before it becomes part of an item: each byte that does not start a valid UTF-8 sequence becomes U+FFFD, so UTF-16 indexes into the string you passed still point at the same places (values nested deeper than 1000 levels are not checked below that depth). An embed or format value that is not a plain value (strings, numbers, bools, nil, `[]byte`, `[]any`, `map[string]any`) is marshaled once, at the call, and stored as that JSON. A value passed to the API belongs to the document from then on: do not mutate it, or a value a read returns, after the call. Updates arriving with invalid UTF-8 are rejected (`ygo.ErrInvalidUTF8`); bytes an older release stored load as they were.

JavaScript's `undefined`, `BigInt` and `-0` keep their identity: they read as `ygo.Undefined{}`, `ygo.BigInt` and a `float64` negative zero, and are written as lib0 writes them, checked byte for byte against yjs in both directions. A BigInt set from Go reaches JavaScript as a BigInt, except in a V1 format attribute or embed: those are JSON, where a BigInt is a Number and loses digits beyond 2^53. Where ygo renders a value as JSON (gomobile), an undefined object entry is left out and an undefined array entry is null, as `JSON.stringify` does; a top-level undefined is null, -0 is 0, and a BigInt, where `JSON.stringify` throws, is a number.

Three differences remain. An object with more than one key comes back with its keys sorted, where yjs keeps their insertion order: clients online when the update arrives get the original bytes, but a client that receives the value from a Go peer's state (a late joiner, or after the server compacts) sees the sorted order. And a whole-number `float64` set from Go is written as a float64, where lib0 writes a varint. A format attribute whose value is `undefined` itself reads as nil (null) and is written back as null, where yjs keeps the undefined.

### Observing changes

Subscribe to a shared type to learn exactly what changed in each transaction, local or remote:

```go
m := ygo.NewMap(d, "settings")
unsub := m.Observe(func(e *ygo.MapEvent) {
    for key, change := range e.Keys {
        // change.Action is "add" / "update" / "delete"; change.OldValue
        // holds the prior value for update / delete.
        fmt.Printf("%s %s\n", change.Action, key)
    }
})
defer unsub()
```

`Array.Observe` and `Text.Observe` deliver a Quill-style delta (`{Insert, Delete, Retain}` ops; Text ops carry formatting attributes). `Map.ObserveDeep` / `Array.ObserveDeep` fire for changes to nested types too, with each event's `Path` from the observed type to the change. The event semantics match `yjs`'s YMapEvent / YArrayEvent / YTextEvent, cross-checked against captured reference output.

## Status

**Stable CRDT core, with known gaps.** The CRDT engine, the V1 and V2 wire formats, and the type set above are validated bidirectionally against `yjs@13.6.33` and exercised in CI on every push to `main`. The public API is considered stable for the v1.x line; changes follow semantic versioning, with new functionality as minor releases and breaking changes deferred to a future major.

### Known gaps

- **Hocuspocus extensions.** Token refresh on an established server key (`onTokenSync`), server-initiated token requests and Redis cluster opcodes are not implemented. Close and reopen a key to change its token or scope; see [server setup](docs/yserve.md#coming-from-hocuspocus).
- **Message size.** The server reads client messages up to 4 MiB by default. A larger one (the first sync of a larger document) closes the connection with 1009, and the client reconnects and resends it in a loop. Raise `ReadLimit` (`yserve -read-limit`) above your largest document.
- **Any values.** An object with more than one key comes back with its keys sorted, and a format attribute set to undefined comes back as null; see [Strings and values](#strings-and-values).
- **Speed with a transaction per edit.** Applying edits one transaction each takes about twice yjs's time on the same machine; with all edits in one transaction ygo is faster than yjs. See [Benchmarks](#benchmarks).

### Status by layer

| Layer | Status |
|---|---|
| `internal/lib0` varint + RLE encoding | done; verified byte-equivalent vs JS `lib0@0.2.119` (40 + 16 fixtures) |
| `internal/block` (Item, Content, Branch, Splice, Integrate-YATA, TrySquash, Repair, search markers) | done; full YATA conflict resolution + per-branch LRU position cache |
| `internal/store` (BlockStore, ItemSlice, Materialize) | done |
| `internal/doc` (Doc, Transaction, TransactionMut) | done; lock semantics + root-branch registry |
| `internal/encoding` (StateVector, IdSet, Update encode/decode/apply, Pending buffer, V1 + V2 codecs) | done; JS Yjs → Go proven by 33 V1 + 37 V2 fixture scenarios; Go → JS proven by 74 reverse fixtures (Map / Array / Text / XmlFragment) |
| `internal/utf16` (UTF-16 length / byte-offset / surrogate-aware split) | done |
| `internal/types/Map` (Set / Get / Delete / Has / Len / Range / Clear + SetMap / SetArray / SetText) | done; nested-type construction supported |
| `internal/types/Array` (Insert / InsertRange / Push / Delete / Get / Len / Range / ToSlice + InsertMap / InsertArray / InsertText) | done; nested-type construction supported |
| `internal/types/Text` (Insert / Delete / String / Length + InsertWithAttributes / Format / InsertEmbed / Range / ToDelta / ApplyDelta) | done; full rich-text + Quill delta batch API |
| Nested-type construction (Map-in-Map, Array-in-Map, etc., to arbitrary depth) | done; ContentType wire format + Repair ParentID resolution + pending-queue retry |
| `internal/types/Xml*` (XmlFragment, XmlElement, XmlText) | done; ProseMirror/Tiptap/BlockNote unblocked. XmlHook (legacy) deferred. |
| Persistence (`Store` interface + `modernc.org/sqlite` reference impl) | done; append-only update log, Flush compaction, LoadDoc / GetStateVector / GetDiff helpers; pure-Go (no CGO). The sqlite backend runs in WAL mode with a busy timeout for concurrency-safe writes |
| Update-level utilities (`MergeUpdates` / `MergeUpdatesV2` / `DiffUpdate` / `EncodeStateVectorFromUpdate`) | done; yjs-parity helpers to merge, diff, and compute a state vector from update bytes, for server-side compaction and diffing. `EncodeStateVectorFromUpdate` reads the bytes directly; the merge and diff helpers reconstruct a document internally. Merging preserves blocks whose causal ancestor is absent from the set, writing `Skip` runs for the holes exactly as yjs does, so an incomplete set is coalesced rather than silently truncated |
| y-sync protocol (`internal/sync`) | y-websocket Sync, Awareness and QueryAwareness; Hocuspocus document routing, Auth scopes, Stateless, Close and SyncStatus. Real providers and servers run in CI. |
| Awareness (`internal/awareness`) | done; LWW presence map, JSON wire payload per y-protocols, self-eviction defense, SweepOutdated, decode caps + per-room client cap + tombstone GC (DoS hardening) |
| `server/` (WebSocket sync server) | done; `http.Handler` mount-anywhere shape, per-doc broadcaster, persists every applied update to optional `persist.Store`. Connection lifecycle hooks (`OnConnect` / `OnDisconnect` / `OnChange` / `OnLoadDocument` / `OnAuthenticate` / `OnAuthenticateRequest` / `OnStateless`), per-key Hocuspocus auth and shared sockets, read-only viewers (`ReadOnly`), resource caps (`MaxConnsPerDoc` / `MaxDocs` / `MaxAwarenessClients`), a send queue and writer per connection so a slow client never delays its room (one that falls behind is closed and resyncs: `MaxSendQueue` / `MaxSendQueueBytes` / `WriteTimeout`), a `Stats` load snapshot, and awareness disconnect tombstones + periodic presence sweep (`AwarenessTimeout`) |
| `server/backplane` (multi-instance clustering) | done; pluggable pub/sub `Backplane` so instances behind a load balancer converge on one document, carrying both document updates and presence (relayed presence held to its own origin-partitioned share of the per-room cap, so a flood on one instance cannot starve local clients on another). In-process `Memory` hub in the core; the opt-in [NATS adapter](server/backplane/nats) is a separate module (so NATS stays out of the core's dependencies) offering core-NATS at-most-once or a JetStream mode that resumes across a broker reconnect or restart. Requires a shared `Store`, and all instances in a cluster must run the same version. Library only: `yserve` has no backplane option |
| [`integration/matrix`](integration/matrix) (federated transport) | done; opt-in separate module carrying updates as `dev.ygo.update` room events over a Matrix room, no ygo server involved. Backward `/messages` pagination from the `/sync` `prev_batch` token (an empty page is not the end of history), per-call event-ID dedup, `ygo.ValidateUpdate` on both publish and read, per-event decoding so one hostile event costs one event rather than a whole page, an event-size ceiling, refusal of end-to-end-encrypted rooms, and a `/messages` fallback when `/sync` is unusable. Unit suite against a strict in-process double plus a real Dendrite in CI on every push to `main` and every pull request. Deliberately thin: whole blobs, no state-vector diffing, no compaction — see its README for what that costs |
| [yserve](docs/yserve.md) (server binary for y-websocket and Hocuspocus clients) | done; one binary with SQLite persistence (`-store`) and periodic document versioning (`-version-interval` / `-keep-versions`); `cmd/ygo-server` remains as a deprecated alias |
| `gomobile/` (app-level mobile SDK for iOS/Android) | done; pure-Go (no CGO) bindable SDK with two levels: the full shared type set — `Text` (UTF-16 mutators), rich text (`ApplyDelta` / `Format` / `InsertEmbed` — Quill deltas in, symmetric with `ObserveChanges`), `Map` and `Array` (typed JSON values, nested types), and `XmlFragment` / `XmlElement` / `XmlText` (ProseMirror / Tiptap) — plus `UndoManager` (tracking the origin of a `WithOrigin` handle, so a collaborator's update applied with `ApplyUpdateWithOrigin` stays out of the user's undo), cursor anchors (relative positions), live collaborator presence (`ObservePresence` / `PresenceStates` delivering remote cursors and identity as JSON), and an embedded sync `Client` (`NewClient` / `Connect` / `Listener`) so a Swift / Kotlin app edits the Doc and renders UI while sync runs in the background; plus the bytes-in/bytes-out wire layer for custom transports, with `ObserveUpdates` delivering each transaction's update. CI binds the Android `.aar` (all four ABIs) on every push to `main` and every pull request. `gomobile bind -target=ios,iossimulator` last ran by hand on 2026-11-10 (Xcode 26.5, Go 1.26.3) and produces `Ygo.xcframework` (arm64 + simulator); the iOS bind is not run in CI (the pure-Go package compiles in CI, guarding against CGO leak). See [gomobile/README.md](gomobile/README.md) for commands. |
| V2 update encoding | done; lib0 RLE primitives + column encoder/decoder + `Update.{EncodeV2,DecodeV2}` + public `ygo.{EncodeStateAsUpdateV2,EncodeDiffV2,ApplyUpdateV2}`; bidirectional cross-language fixtures vs `yjs@13.6.33` |
| Untrusted-input hardening | done; every wire-supplied element count in the V1/V2 update, snapshot, id-set, and Any-content decoders is bounded against the input length, closing a length-prefix amplification DoS (a few bytes forcing a multi-terabyte allocation). Continuous fuzzing of the decode + apply paths runs nightly in CI ([fuzz.yml](.github/workflows/fuzz.yml)) with the discovered crashers committed as regression corpus |
| dmonad/crdt-benchmarks B1-B4 port | done; B1.1-B1.11 / B2.1-B2.4 / B3.1+3+4 / B4 (260k-edit real-world LaTeX trace). Baseline in [BENCHMARKS.md](BENCHMARKS.md). |
| `UndoManager` (`internal/undo`) | done; scoped Undo / Redo over Map / Array / Text with capture-timeout grouping, tracked-origin filtering (`AddTrackedOrigin` / `RemoveTrackedOrigin`), and a `Redone` chain for deletion restore, deleted nested types included: the type comes back as a new copy with its content, as in yjs. A subdocument reference is not restored, on its own or inside a deleted type, where yjs restores a new reference with the same GUID and options. Cross-language conformance vs `yjs@13.6.33`: 27 final-state scenarios, plus 46 undo scenarios in the update-event set, nested types above all, that check every update, return value and stack state on the way |
| Per-transaction updates (`Doc.OnUpdate` / `OnUpdateV2`, `ApplyUpdateWithOrigin`) | done; the yjs `update` / `updateV2` events, emitted after GC with the transaction's origin (byte for byte apart from the pinned cases here and the Any values listed under [Strings and values](#strings-and-values)); `EncodeDiff` / `EncodeDiffV2` cut the first block at the remote clock as yjs does, but leave out the pending buffer, which yjs appends. 112 cross-language scenarios, including the format cleanup yjs runs after a remote transaction and updates whose blocks wait on each other, each checked for the order of the types every transaction changes and replayed as V1, V2 and both by turns, 1 pinned divergence (a root first read as text after formatted text arrived inserts with the attributes in effect, where yjs can insert at a search marker without them) |
| Snapshots (`CreateSnapshot` / `EncodeSnapshot` / `RestoreSnapshot`) | done; V1 wire format byte-compatible with `yjs@13.6.33` (cross-language fixtures incl. multi-client), `RestoreSnapshot` mirrors `Y.createDocFromSnapshot` |
| Subdocuments (`Map.SetDoc` / `Map.GetDoc`) | done; `ContentDoc` wire format (GUID + options) byte-compatible with `yjs@13.6.33`, cross-language fixtures. Lifecycle events via OnSubdocs / autoLoad / Load |
| Wire client-ID width | 53-bit client IDs throughout (`uint64` + varint), byte-verified against `yjs@13.6.33` for IDs above 2^32. Forward-compatible with the wider client-ID space yjs@14 introduces |
| Commit-time block squash | done; merges same-client adjacent-clock items at commit (~1 byte/char V1), paired with Apply-side partial-overlap slicing for correct remote integration of merged blocks |
| GC merging | done; deleted content is freed at commit (ContentDeleted, byte-aligned with yjs) and adjacent deleted runs are merged. Deleting a nested shared type recursively collapses its whole subtree into garbage-collected runs (cross-language fixtures), matching yjs. Skipped when GC is disabled or for items an UndoManager keeps |
| Relative positions (cursors) | done; `CreateRelativePositionFromTypeIndex` / `CreateAbsolutePositionFromRelativePosition`, binary form byte-compatible with `Y.encodeRelativePosition`, follows undone deletions; cross-language fixtures incl. surrogate pairs and 53-bit client IDs |
| Versioned persistence | done; named point-in-time versions independent of the live log (`persist.VersionStore`: save / list / load / restore / prune), atomic restore, sqlite reference implementation |
| Change observers | done; `Map.Observe` (YMapEvent: add / update / delete + oldValue), `Array.Observe` and `Text.Observe` (Quill-style insert / delete / retain delta, Text formatting-aware), `Map`/`Array.ObserveDeep` (event-path bubbling from nested types). Deltas cross-checked against captured `yjs@13.6.33` output; fire on local and remote transactions, in first-change order, with one deep call per transaction, checked against yjs by 19 observer scenarios. gomobile `Text`/`Map.ObserveChanges` deliver the delta as Quill JSON |

## Goals

1. **Binary protocol compatibility** with [Yjs](https://github.com/yjs/yjs) v13.x in both V1 and V2 wire formats. Byte-for-byte. JS clients sync with Go servers and vice versa, bidirectionally verified.
2. **Idiomatic Go API.** Callbacks for events, explicit transactions, `error` returns.
3. **Pure Go.** No CGO. `gomobile bind` works for iOS and Android.
4. **Pluggable persistence** with `modernc.org/sqlite` reference implementation.
5. **Performance within 2× of [yrs](https://github.com/y-crdt/y-crdt)** on `dmonad/crdt-benchmarks` B1-B4. Not verified: no run against native yrs exists. On B4, ygo takes 0.7 times yjs's time with all edits in one transaction and about twice with a transaction per edit, on the same machine. See [Benchmarks](#benchmarks).

## Non-goals

- C-FFI surface. [Yrs](https://github.com/y-crdt/y-crdt) already provides this; Ygo's unique value is pure-Go native binaries.
- Drop-in replacement for the Node.js Yjs runtime. Ygo is the Go port; use `yjs` itself if you want a JavaScript runtime.
- Loro, Automerge, RGA, or other CRDT designs. Ygo implements the Yjs wire format, period.

## Wire compatibility

The single most-important guarantee of this project is byte-level wire compatibility with `yjs@13.x`. This is enforced by **307 cross-language fixture scenarios** (plus 56 lib0 primitive vectors):

- **33 V1 forward fixtures** (`testdata/yjs-updates.json`) — JS Yjs encodes via `Y.encodeStateAsUpdate`, Go decodes and applies, state matches.
- **37 V2 forward fixtures** (`testdata/yjs-update-v2-fixtures.json`) — same with `Y.encodeStateAsUpdateV2`.
- **74 reverse fixtures** (`testdata/go-updates.json` + `go-update-v2-fixtures.json`) — Go encodes via `EncodeStateAsUpdate` / `EncodeStateAsUpdateV2`, JS Yjs decodes via `Y.applyUpdate` / `Y.applyUpdateV2`, state matches.
- **163 feature fixtures** — XML (5), awareness (6), sync protocol (6), undo (27), snapshots (4), subdocuments (3), wire edge cases incl. 53-bit client IDs (3), nested-type GC (4), relative positions (11), observers (19, which observers fire and in what order), rich text (55, what inserts, embeds, formatting, deletes and Quill deltas do to a formatted text, compared as deltas, strings and events, not as bytes; the update-event set below compares rich-text updates byte for byte), Any values (20: undefined, BigInt and -0 as map values, inside arrays and objects, in a subdocument's options, and in V2 format attributes and embeds, checked as values and byte for byte both ways), all captured from the pinned JS reference and byte-compared in both directions where the feature has a Go encoder.

One more fixture set compares bytes with yjs but stays out of these totals while any of its scenarios is a pinned divergence: per-transaction updates and diffs (112 scenarios, every `OnUpdate` / `OnUpdateV2` event and `EncodeDiff` / `EncodeDiffV2` against state vectors that end inside a block, undo of nested types, rich-text inserts and deltas, and the format cleanup after remote transactions among them; 1 pinned). Each pin is listed with its cause in the test and fails the build when the behaviour changes, in either direction.

The fixtures regenerate from pinned `yjs@13.6.33` + `lib0@0.2.119` + `y-protocols@1.0.7` on every CI run; `git diff --exit-code testdata/` catches byte-level regressions.

Byte equality does not extend to objects with more than one key, whose keys ygo sorts, to whole-number float64s set from Go, or to a format attribute set to undefined, which ygo writes back as null (see [Strings and values](#strings-and-values)).

## How is this different from Hocuspocus / y-websocket / y-leveldb?

| Project | Runtime | What it provides | Relationship to Ygo |
|---|---|---|---|
| `yjs` (npm) | Node / browser | The reference CRDT implementation | Ygo's wire-format target |
| `y-websocket` | Node | Reference WebSocket server | [yserve](docs/yserve.md) is a Go-native equivalent |
| `Hocuspocus` | Node | Production WebSocket server with auth, persistence, extensions | The Go client syncs with it (servers 2.15, 3.4, 4.8; `client.Options.Hocuspocus`); yserve serves providers 2.15, 3.4 and 4.8 through its base endpoint |
| `yrs` | Rust | Reference Rust port | Ygo's executable spec for porting decisions |
| `y-leveldb`, `y-indexeddb` | Node / browser | Persistence backends | Ygo's `persist/sqlite` is a Go-native equivalent |
| **Ygo** | **Go** | **CRDT engine + WS server + persistence in one monorepo, pure-Go for native mobile** | **This project** |

If you run a `y-websocket` deployment and want to move the server side to Go (no Node runtime, one binary), ygo is an option; read the [known gaps](#known-gaps) first. Hocuspocus providers can connect to ygo's Hocuspocus endpoint; server extensions and authentication hooks need to be ported to Go. If you're starting fresh and your team is comfortable with Node, Hocuspocus is the mature choice.

## Benchmarks

See [BENCHMARKS.md](BENCHMARKS.md) for the full table and a same-machine comparison with [reearth/ygo](https://github.com/reearth/ygo) v1.55.0, another Go port. B4 (259,778-edit real-world LaTeX paper trace) on one Apple M3, measured on 2026-11-11 with ygo 1.38.0 on Go 1.26.3 and yjs 13.6.33 on Node 26.6, all edits in one transaction with client ID 1, fastest of three interleaved runs:

| Metric | Ygo | yjs |
|---|---|---|
| Apply all edits | 0.88 s | 1.22 s |
| Encoded doc size, V1 | 223 KB | 223 KB |
| Encoded doc size, V2 | 160 KB | 160 KB |
| Encode V1 / V2 | 0.55 ms / 0.94 ms | 1.7 ms / 1.6 ms |

**How to read this:**

- **All edits in one transaction** take 0.7 times yjs's time. **A transaction per edit** with a random client ID and an update handler, which is closer to real use, takes 2.1-2.4 s against 1.1 s.
- **Document sizes match yjs**, with a random client ID too: both draw 32-bit IDs, and the V1 document takes 311 KB and the V2 160 KB in both.

No head-to-head run against native yrs exists.

## Roadmap

Shipped since v1.0 (June 2026): a sync server with connection lifecycle hooks, read-only viewers, resource caps and a stats snapshot, horizontal scaling across instances with cross-cluster cursors, an offline-first Go client, WAL-backed SQLite persistence, continuous fuzzing, the yjs update-level utilities, and per-transaction update events with origins (1.22.0). See the [CHANGELOG](CHANGELOG.md), the [releases](https://github.com/Deln0r/ygo/releases), and [examples/](examples).

Open: the [known gaps](#known-gaps) above, including a documentation site, an external security audit, and yjs v14 wire features (attribution / IdMap) once v14 reaches GA.

Per-layer port notes live in [docs/yrs-port-notes/](docs/yrs-port-notes/). Items intentionally deferred or partial are tracked in [docs/tech-debt.md](docs/tech-debt.md). Detailed design decisions in [DESIGN.md](DESIGN.md).

## Examples

Runnable examples in [examples/](examples):

- [collab-server](examples/collab-server) — embed the WebSocket sync server and wire its hooks (`OnConnect`, `ReadOnly`, `OnChange`, `OnLoadDocument`), resource caps, a SQLite store, and a `/stats` endpoint.
- [collab-client](examples/collab-client) — a Go-native sync client: connect, observe remote changes, edit, converge.
- [offline-first](examples/offline-first) — client-side offline persistence with a `LocalStore`: usable with no network, edits survive restarts and sync up on reconnect.

The core CRDT API also has output-verified [runnable examples on pkg.go.dev](https://pkg.go.dev/github.com/Deln0r/ygo#pkg-examples) (Map, Array, Text, two-document sync, per-transaction updates, UndoManager).

## Documentation

- [DESIGN.md](DESIGN.md) — project design document
- [CHANGELOG.md](CHANGELOG.md) — release history, with an upgrade-impact note on every release that changes what a running deployment does
- [BENCHMARKS.md](BENCHMARKS.md) — performance baseline + B1-B4 methodology
- [gomobile/README.md](gomobile/README.md) — iOS xcframework + Android AAR build instructions
- [docs/yrs-port-notes/](docs/yrs-port-notes/) — per-layer port notes describing how each yrs subsystem maps to Go
- [integration/matrix/README.md](integration/matrix/README.md) — the Matrix transport: wire format, what is measured against a real homeserver, and its limits
- [docs/tech-debt.md](docs/tech-debt.md) — deferred work and known limitations
- [CONTRIBUTING.md](CONTRIBUTING.md) — DCO sign-off, no CLA

## License

MIT. See [LICENSE](LICENSE).

## Acknowledgements

- [Kevin Jahns (dmonad)](https://github.com/dmonad) for [Yjs](https://github.com/yjs/yjs) and the YATA algorithm.
- [Bartosz Sypytkowski](https://www.bartoszsypytkowski.com/) for [yrs](https://github.com/y-crdt/y-crdt) and the [architecture deep dive](https://www.bartoszsypytkowski.com/yrs-architecture/).
