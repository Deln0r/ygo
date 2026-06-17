# Ygo

[![CI](https://github.com/Deln0r/ygo/actions/workflows/test.yml/badge.svg)](https://github.com/Deln0r/ygo/actions/workflows/test.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/Deln0r/ygo.svg)](https://pkg.go.dev/github.com/Deln0r/ygo)
[![Go Report Card](https://goreportcard.com/badge/github.com/Deln0r/ygo?cache=v1.2.0)](https://goreportcard.com/report/github.com/Deln0r/ygo)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![Go Version](https://img.shields.io/badge/go-1.22%2B-00ADD8.svg)](go.mod)
[![Yjs Protocol](https://img.shields.io/badge/Yjs%20protocol-V1%20%2B%20V2-7c3aed.svg)](https://github.com/yjs/yjs)
[![Live Demo](https://img.shields.io/badge/live%20demo-ygo.deln0r.com-22c55e.svg)](https://ygo.deln0r.com)
[![Codeberg Mirror](https://img.shields.io/badge/codeberg-mirror-2185d0?logo=codeberg&logoColor=white)](https://codeberg.org/Deln0r/ygo)

Pure-Go port of [Yjs](https://github.com/yjs/yjs), the CRDT framework for collaborative applications.

Ygo speaks the **Yjs V1 and V2 wire formats byte-for-byte**. JavaScript clients running `yjs@13.x` synchronize directly with Go servers and vice versa, with both directions verified through **158 cross-language fixture scenarios** generated from `yjs@13.6.31`. The bundled WebSocket server is Hocuspocus-compatible. No CGO; `gomobile bind` produces an iOS xcframework and Android AAR (manually verified, not run in CI).

## Highlights

- **Byte-for-byte wire compatibility, verified in both directions.** 158 cross-language fixture scenarios (generated from `yjs@13.6.31`) cover the V1 and V2 update formats, snapshots, subdocuments, undo, relative positions, GC, awareness, and the sync protocol, JS to Go and Go to JS, plus 56 lib0 primitive vectors. The suite runs in CI on every push, so a regression in either direction fails the build.
- **Pure Go, no CGO — mobile included.** Builds for any Go target, compiles to WASM, and cross-compiles freely. `gomobile bind` produces an iOS xcframework and Android AAR (manually verified on 2026-06-12, not run in CI) carrying a full mobile SDK: editable Text / Map, undo, cursors, and a built-in background sync client (WebSocket + reconnect), so a Swift / Kotlin app only renders UI. No V8, no embedded JavaScript engine, no Rust FFI bridge.
- **Embeddable sync client, offline-first.** The [`client`](client) package is a Go-native y-websocket/Hocuspocus provider: handshake, incremental updates, awareness, reconnect with backoff. With a `LocalStore` it persists the document to disk (pure-Go SQLite), loads it before any network so the app works offline, and syncs edits made offline up on reconnect. The building block for bots, CLI tools, server-side agents, and the mobile SDK (`EnableOfflineStore`).
- **Complete CRDT type set.** Map, Array, Text (rich-text formatting, Quill deltas, embeds), XML types, Awareness, UndoManager, Snapshots / time-travel, and Subdocuments.
- **Change observers.** `Map.Observe` / `Array.Observe` / `Text.Observe` deliver Quill-style deltas of exactly what changed; `ObserveDeep` bubbles events from nested types with their path. Semantic parity with yjs's YMapEvent / YArrayEvent / YTextEvent. On mobile, `ObserveChanges` hands a native editor the delta as JSON.
- **Compact encoding.** Commit-time block squash collapses per-character edits into single items (about 1 byte per character in V1), and garbage collection frees deleted content at commit. On a real-world editing trace V1 document size drops from ~1.97 MB to ~223 KB, competitive with V2.
- **Forward-looking wire handling.** ygo already handles both confirmed wire-level changes in the `yjs@14` release candidate: 53-bit client IDs throughout (byte-verified above 2^32) and Skip structs in the update stream (decoded as no-op gaps). Full attribution / IdMap support waits for the v14 format to stabilize.
- **Ready-to-run server: [yserve](docs/yserve.md).** A self-hosted Yjs server in a single static binary — a Hocuspocus alternative with no Node, no Redis, no CGO. Same wire protocol, so existing `@hocuspocus/provider` / `y-websocket` clients connect unchanged; SQLite persistence and periodic document versioning built in. Also embeds as a plain `http.Handler` inside an existing Go backend.
- **EU-sovereign mirror** on [codeberg.org/Deln0r/ygo](https://codeberg.org/Deln0r/ygo), auto-synced from GitHub on every push for adopters who prefer or require EU-hosted code infrastructure.

**Live demo:** open [ygo.deln0r.com](https://ygo.deln0r.com) in two browser tabs and start typing. Same protocol any standard Yjs ecosystem client speaks, with a pure-Go server behind it.

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

For a collaborative server backend, see [yserve](docs/yserve.md) — a stand-alone, Hocuspocus-compatible WebSocket server with SQLite persistence and document versioning, in one static binary:

```bash
go run ./cmd/yserve -addr :1234 -store data.db -version-interval 10m
```

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

Only local edits under the watched types are captured; remote updates applied via `ApplyUpdate` are not. Rapid edits inside the capture-timeout window collapse into one undo step; call `um.StopCapturing()` to force a boundary. The semantics match `yjs@13.6.31`'s `UndoManager`, checked by cross-language conformance fixtures.

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

The snapshot wire format (`EncodeSnapshot` / `DecodeSnapshot`) is byte-compatible with `yjs@13.6.31`'s `Y.encodeSnapshot`, verified by cross-language fixtures including multi-client delete-set ordering. `RestoreSnapshot` mirrors `Y.createDocFromSnapshot`.

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

The `ContentDoc` wire format (GUID + options) is byte-compatible with `yjs@13.6.31`, verified by cross-language fixtures. Lifecycle events are observable via `d.OnSubdocs` (added / removed / loaded GUIDs per transaction); `SetDocWithOptions(..., autoLoad)` and `subdoc.Load()` drive the loaded set, so a sync provider knows which nested documents to fetch.

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

**Feature-complete and stable.** The CRDT engine, the V1 and V2 wire formats, and the full type set above are validated bidirectionally against `yjs@13.6.31` and exercised in CI on every push. The public API is considered stable for the v1.x line; changes follow semantic versioning, with new functionality as minor releases and breaking changes deferred to a future major.

| Layer | Status |
|---|---|
| `internal/lib0` varint + RLE encoding | done; verified byte-equivalent vs JS `lib0@0.2.117` (40 + 16 fixtures) |
| `internal/block` (Item, Content, Branch, Splice, Integrate-YATA, TrySquash, Repair, search markers) | done; full YATA conflict resolution + per-branch LRU position cache |
| `internal/store` (BlockStore, ItemSlice, Materialize) | done |
| `internal/doc` (Doc, Transaction, TransactionMut) | done; lock semantics + root-branch registry |
| `internal/encoding` (StateVector, IdSet, Update encode/decode/apply, Pending buffer, V1 + V2 codecs) | done; JS Yjs → Go proven by 29 V1 + 32 V2 fixture scenarios; Go → JS proven by 48 reverse fixtures (Map / Array / Text / XmlFragment) |
| `internal/utf16` (UTF-16 length / byte-offset / surrogate-aware split) | done |
| `internal/types/Map` (Set / Get / Delete / Has / Len / Range / Clear + SetMap / SetArray / SetText) | done; nested-type construction supported |
| `internal/types/Array` (Insert / InsertRange / Push / Delete / Get / Len / Range / ToSlice + InsertMap / InsertArray / InsertText) | done; nested-type construction supported |
| `internal/types/Text` (Insert / Delete / String / Length + InsertWithAttributes / Format / InsertEmbed / Range / ToDelta / ApplyDelta) | done; full rich-text + Quill delta batch API |
| Nested-type construction (Map-in-Map, Array-in-Map, etc., to arbitrary depth) | done; ContentType wire format + Repair ParentID resolution + pending-queue retry |
| `internal/types/Xml*` (XmlFragment, XmlElement, XmlText) | done; ProseMirror/Tiptap/BlockNote unblocked. XmlHook (legacy) deferred. |
| Persistence (`Store` interface + `modernc.org/sqlite` reference impl) | done; append-only update log, Flush compaction, LoadDoc / GetStateVector / GetDiff helpers; pure-Go (no CGO) |
| y-sync protocol (`internal/sync`) | done; full Hocuspocus message subset (Sync + Awareness + QueryAwareness + Auth + Stateless + BroadcastStateless + Close + SyncStatus); per-document Auth permission scoping deferred ([tech-debt](docs/tech-debt.md)) |
| Awareness (`internal/awareness`) | done; LWW presence map, JSON wire payload per y-protocols, self-eviction defense, SweepOutdated, decode caps + per-room client cap + tombstone GC (DoS hardening) |
| `server/` (WebSocket sync server) | done; `http.Handler` mount-anywhere shape, per-doc broadcaster, persists every applied update to optional `persist.Store`, awareness disconnect tombstones + periodic presence sweep (`AwarenessTimeout` / `MaxAwarenessClients`) |
| [yserve](docs/yserve.md) (Hocuspocus-compat server binary) | done; single static binary with SQLite persistence (`-store`) and periodic document versioning (`-version-interval` / `-keep-versions`); `cmd/ygo-server` remains as a deprecated alias |
| `gomobile/` (app-level mobile SDK for iOS/Android) | done; pure-Go (no CGO) bindable SDK with two levels: app-level editable `Text` / `Map` (mutators, UTF-16 indices), `UndoManager`, cursor anchors (relative positions), live collaborator presence (`ObservePresence` / `PresenceStates` delivering remote cursors and identity as JSON), and an embedded sync `Client` (`NewClient` / `Connect` / `Listener`) so a Swift / Kotlin app edits the Doc and renders UI while sync runs in the background; plus the bytes-in/bytes-out wire layer for custom transports. `gomobile bind` was manually verified once (2026-06-12, Xcode 16 + NDK 27 + Go 1.26) to produce a valid `Ygo.xcframework` (arm64 + simulator universal) and Android `.aar` (arm64-v8a / armeabi-v7a / x86 / x86_64); the bind step is not run in CI (the pure-Go package compiles in CI, guarding against CGO leak). See [gomobile/README.md](gomobile/README.md) for commands. |
| V2 update encoding | done; lib0 RLE primitives + column encoder/decoder + `Update.{EncodeV2,DecodeV2}` + public `ygo.{EncodeStateAsUpdateV2,EncodeDiffV2,ApplyUpdateV2}`; bidirectional cross-language fixtures vs `yjs@13.6.31` |
| Untrusted-input hardening | done; every wire-supplied element count in the V1/V2 update, snapshot, id-set, and Any-content decoders is bounded against the input length, closing a length-prefix amplification DoS (a few bytes forcing a multi-terabyte allocation). Continuous fuzzing of the decode + apply paths runs nightly in CI ([fuzz.yml](.github/workflows/fuzz.yml)) with the discovered crashers committed as regression corpus |
| dmonad/crdt-benchmarks B1-B4 port | done; B1.1-B1.11 / B2.1-B2.4 / B3.1+3+4 / B4 (260k-edit real-world LaTeX trace). Baseline in [BENCHMARKS.md](BENCHMARKS.md). |
| `UndoManager` (`internal/undo`) | done; scoped Undo / Redo over Map / Array / Text with capture-timeout grouping, tracked-origin filtering, and a `Redone` chain for deletion restore. Cross-language conformance vs `yjs@13.6.31` (7 scenarios) |
| Snapshots (`CreateSnapshot` / `EncodeSnapshot` / `RestoreSnapshot`) | done; V1 wire format byte-compatible with `yjs@13.6.31` (cross-language fixtures incl. multi-client), `RestoreSnapshot` mirrors `Y.createDocFromSnapshot` |
| Subdocuments (`Map.SetDoc` / `Map.GetDoc`) | done; `ContentDoc` wire format (GUID + options) byte-compatible with `yjs@13.6.31`, cross-language fixtures. Lifecycle events via OnSubdocs / autoLoad / Load |
| Wire client-ID width | 53-bit client IDs throughout (`uint64` + varint), byte-verified against `yjs@13.6.31` for IDs above 2^32. Forward-compatible with the wider client-ID space yjs@14 introduces |
| Commit-time block squash | done; merges same-client adjacent-clock items at commit (~1 byte/char V1), paired with Apply-side partial-overlap slicing for correct remote integration of merged blocks |
| GC merging | done; deleted content is freed at commit (ContentDeleted, byte-aligned with yjs) and adjacent deleted runs are merged. Deleting a nested shared type recursively collapses its whole subtree into garbage-collected runs (cross-language fixtures), matching yjs. Skipped when GC is disabled or for items an UndoManager keeps |
| Relative positions (cursors) | done; `CreateRelativePositionFromTypeIndex` / `CreateAbsolutePositionFromRelativePosition`, binary form byte-compatible with `Y.encodeRelativePosition`, follows undone deletions; cross-language fixtures incl. surrogate pairs and 53-bit client IDs |
| Versioned persistence | done; named point-in-time versions independent of the live log (`persist.VersionStore`: save / list / load / restore / prune), atomic restore, sqlite reference implementation |
| Change observers | done; `Map.Observe` (YMapEvent: add / update / delete + oldValue), `Array.Observe` and `Text.Observe` (Quill-style insert / delete / retain delta, Text formatting-aware), `Map`/`Array.ObserveDeep` (event-path bubbling from nested types). Deltas cross-checked against captured `yjs@13.6.31` output; fire on local and remote transactions. gomobile `Text`/`Map.ObserveChanges` deliver the delta as Quill JSON |

## Goals

1. **Binary protocol compatibility** with [Yjs](https://github.com/yjs/yjs) v13.x in both V1 and V2 wire formats. Byte-for-byte. JS clients sync with Go servers and vice versa, bidirectionally verified.
2. **Idiomatic Go API.** Channels for events, explicit transactions, `error` returns.
3. **Pure Go.** No CGO. `gomobile bind` works for iOS/Android.
4. **Pluggable persistence** with `modernc.org/sqlite` reference implementation.
5. **Performance within 2× of [yrs](https://github.com/y-crdt/y-crdt)** on `dmonad/crdt-benchmarks` B1-B4. See [BENCHMARKS.md](BENCHMARKS.md).

## Non-goals

- C-FFI surface. [Yrs](https://github.com/y-crdt/y-crdt) already provides this; Ygo's unique value is pure-Go native binaries.
- Drop-in replacement for the Node.js Yjs runtime. Ygo is the Go port; use `yjs` itself if you want a JavaScript runtime.
- Loro, Automerge, RGA, or other CRDT designs. Ygo implements the Yjs wire format, period.

## Wire compatibility

The single most-important guarantee of this project is byte-level wire compatibility with `yjs@13.x`. This is enforced by **158 cross-language fixture scenarios** (plus 56 lib0 primitive vectors):

- **29 V1 forward fixtures** (`testdata/yjs-updates.json`) — JS Yjs encodes via `Y.encodeStateAsUpdate`, Go decodes and applies, state matches.
- **32 V2 forward fixtures** (`testdata/yjs-update-v2-fixtures.json`) — same with `Y.encodeStateAsUpdateV2`.
- **48 reverse fixtures** (`testdata/go-updates.json` + `go-update-v2-fixtures.json`) — Go encodes via `EncodeStateAsUpdate` / `EncodeStateAsUpdateV2`, JS Yjs decodes via `Y.applyUpdate` / `Y.applyUpdateV2`, state matches.
- **49 feature fixtures** — XML (5), awareness (6), sync protocol (6), undo (7), snapshots (4), subdocuments (3), wire edge cases incl. 53-bit client IDs (3), nested-type GC (4), relative positions (11), all captured from the pinned JS reference and byte-compared in both directions where the feature has a Go encoder.

The fixtures regenerate from pinned `yjs@13.6.31` + `lib0@0.2.117` + `y-protocols@1.0.7` on every CI run; `git diff --exit-code testdata/` catches byte-level regressions.

## How is this different from Hocuspocus / y-websocket / y-leveldb?

| Project | Runtime | What it provides | Relationship to Ygo |
|---|---|---|---|
| `yjs` (npm) | Node / browser | The reference CRDT implementation | Ygo's wire-format target |
| `y-websocket` | Node | Reference WebSocket server | [yserve](docs/yserve.md) is a Go-native equivalent |
| `Hocuspocus` | Node | Production WebSocket server with auth, persistence, extensions | [yserve](docs/yserve.md) speaks the same 8-message envelope (Sync / Awareness / QueryAwareness / Auth / Stateless / BroadcastStateless / Close / SyncStatus) in one static binary |
| `yrs` | Rust | Reference Rust port | Ygo's executable spec for porting decisions |
| `y-leveldb`, `y-indexeddb` | Node / browser | Persistence backends | Ygo's `persist/sqlite` is a Go-native equivalent |
| **Ygo** | **Go** | **CRDT engine + WS server + persistence in one monorepo, pure-Go for native mobile** | **This project** |

If you have an existing Yjs deployment and want to move the server side to Go (no Node runtime, single static binary, native iOS / Android via gomobile) — Ygo is the path. If you're starting fresh and your team is comfortable with Node, Hocuspocus is the mature choice.

## Benchmarks

See [BENCHMARKS.md](BENCHMARKS.md) for the full table. Highlights from B4 (259,778-edit real-world LaTeX paper trace) on Apple M3, Go 1.26:

| Metric | Ygo V1 | Ygo V2 | yjs (Node, Intel i5-8400) | ywasm (Intel i5-8400) |
|---|---|---|---|---|
| Apply all edits | 20.3 s | 20.3 s | 5.7 s | 28.7 s |
| Encoded doc size | **223 KB** | **160 KB** | 160 KB | 160 KB |
| Encode time | 0.6 ms | 10.5 ms | 11 ms | 3 ms |
| Parse time | 4.4 ms | 4.5 ms | 39 ms | 16 ms |

**How to read this:**

- **Doc sizes are now at or near yjs parity.** V1 lands within 1.4× of yjs (223 KB vs 160 KB; it was 1.97 MB, ~12× bloat, before commit-time block squash + GC shipped). V2 matches yjs byte-for-byte-scale at 160 KB. A 2,000-character sequential insert encodes to ~1.0 byte/char in V1. Squash ships with the paired Apply-side partial-overlap handling that keeps remote integration correct when a peer sends a merged block overlapping the receiver's state.
- **Apply throughput is the trade.** ~3.5× yjs wall-clock on different hardware (Apple M3 vs the slower i5-8400, so the normalized gap is wider): the per-commit squash + GC scans that buy the 8.8× smaller documents roughly doubled apply time versus the pre-squash build. Against yrs's published sub-10-s B4 numbers this sits at about 2×, at the DESIGN.md target boundary, with known commit-pipeline optimization headroom. (ywasm is yrs compiled to WebAssembly and is not representative of native yrs — wasm overhead inflates it ~5×.)
- **Encode and parse are fast once the doc is compact:** V1 encode 0.6 ms and parse 4.4 ms on the 223 KB document, both ahead of yjs's published numbers on its hardware.

A direct head-to-head harness against native yrs under identical hardware is on the roadmap but not yet run; the numbers above are honest absolute figures with hardware caveats.

## Roadmap

Towards v1.0: benchmarks refresh · documentation site · external security audit. (Undo manager, Snapshots, Subdocuments, commit-time block squash, GC merging: done.)

Per-layer port notes live in [docs/yrs-port-notes/](docs/yrs-port-notes/). Items intentionally deferred or partial are tracked in [docs/tech-debt.md](docs/tech-debt.md). Detailed design decisions in [DESIGN.md](DESIGN.md).

## Documentation

- [DESIGN.md](DESIGN.md) — project design document
- [BENCHMARKS.md](BENCHMARKS.md) — performance baseline + B1-B4 methodology
- [gomobile/README.md](gomobile/README.md) — iOS xcframework + Android AAR build instructions
- [docs/yrs-port-notes/](docs/yrs-port-notes/) — per-layer port notes describing how each yrs subsystem maps to Go
- [docs/tech-debt.md](docs/tech-debt.md) — deferred work and known limitations
- [CONTRIBUTING.md](CONTRIBUTING.md) — DCO sign-off, no CLA

## License

MIT. See [LICENSE](LICENSE).

## Acknowledgements

- [Kevin Jahns (dmonad)](https://github.com/dmonad) for [Yjs](https://github.com/yjs/yjs) and the YATA algorithm.
- [Bartosz Sypytkowski](https://www.bartoszsypytkowski.com/) for [yrs](https://github.com/y-crdt/y-crdt) and the [architecture deep dive](https://www.bartoszsypytkowski.com/yrs-architecture/).
