# DESIGN

> Project design document. Locked unless major reassessment.

## Vision

Pure-Go implementation of the Yjs CRDT framework, binary-protocol compatible with the npm `yjs` package. JavaScript clients sync seamlessly with Go servers. Pure-Go (no CGO) means `gomobile bind` works for iOS/Android.

## Prime directive

**Wire-format compatibility with `yjs` v13.x V1 update encoding is non-negotiable.** Without round-tripping `Y.encodeStateAsUpdate()` and `Y.applyUpdate()` against the JS reference, the project has no reason to exist.

## MVP scope

| Component | In MVP |
|---|---|
| `internal/lib0` varint encoding | Yes |
| `Doc` (transactions, observer events) | Yes |
| `Map`, `Array`, `Text` (plain) shared types | Yes |
| State Vector + V1 update encoding | Yes |
| y-sync protocol (sync step 1/2 + update) | Yes |
| Awareness CRDT | Yes |
| Block squashing | Yes |
| `Store` interface + `modernc.org/sqlite` impl | Yes |
| `y-websocket`-compatible WebSocket server (`cmd/ygo-server`) | Yes |
| `gomobile bind` build target | Yes |

## v1.0 scope

| Component | Notes |
|---|---|
| `Text` rich-text formatting attributes | For ProseMirror/Tiptap. |
| `XmlElement`, `XmlFragment`, `XmlText` | For ProseMirror/Tiptap. |
| V2 update encoding | yrs deferred too. |
| Garbage collection of deleted items | MVP is tombstone-only. |
| Snapshots / time-travel | Defer. |
| Sub-documents | Defer. |
| Undo/Redo manager | Origin-aware transaction model. |

## Out of scope

- C-FFI surface (`yffi`-equivalent). yrs already provides.
- Non-Yjs encoding formats (Loro, Automerge, RGA).
- WASM build.

## Wire-format-driven storage decisions

Two decisions are non-negotiable because they govern byte-level compatibility with the JS reference. Getting either wrong causes silent divergence on the wire after the first non-ASCII character or first concurrent typing burst, with no compile-time warning.

### Text storage: UTF-16 code units internally, opaque at the API edge

The JS Yjs `Item.split` operates on UTF-16 code units (the JavaScript `String` indexing unit). Two replicas exchanging V1 updates must agree on what "one character" means at the wire — otherwise IDs assigned to characters past the first non-BMP codepoint disagree, and both replicas diverge.

- Internal storage of `Text` content: **UTF-16 code units (`[]uint16`)**, with `Item` length expressed in code units.
- Public API `Text.Insert(pos int, s string)` accepts standard Go UTF-8 strings; the implementation re-encodes to UTF-16 at the boundary. Index `pos` is in UTF-16 code units to match JS `Y.Text` semantics, with helpers planned for code-point-based positions.
- This is intentionally awkward Go in exchange for byte equality with the JS encoder. There is no other consistent choice.

See `docs/yjs-architecture-notes.md` §19.

### Item content as slice, not value

Yjs collapses runs of consecutive character inserts by the same client at adjacent clocks into a single `Item` whose `Content` holds an N-length sequence. Single-element Items are an anti-optimization: the runtime immediately wants to merge them, and the integration algorithm assumes `Item.split(at)` exists.

- `block.Content` is a tagged union where the `String` and similar variants hold a slice (`[]uint16`, `[]any`), not a single value.
- `block.Item.Split(at uint64) (left, right *Item)` is implemented before any insertion logic; integration depends on it.
- The Item's `len` field counts elements (clock units), independent of the in-memory representation.

See `docs/yjs-architecture-notes.md` §3.2.

## API style

Idiomatic Go signatures with JS-recognizable type names.

```go
doc := ygo.NewDoc()
defer doc.Destroy()

txn := doc.WriteTxn()
defer txn.Commit()

m := doc.Map("settings")
m.Set(txn, "theme", "dark")

events := m.Observe()
go func() {
    for ev := range events {
        // handle event
    }
}()
```

- `NewDoc()`, not `NewYDoc()`
- Explicit transactions (mirror yrs's `transact_mut`)
- Callbacks for events
- `error` returns, no panics
- `context.Context` on long-running operations
- Map keys: `string` only
- Map/Array values: `any` with documented conversion rules

## Concurrency

- One `sync.RWMutex` per `Doc`.
- Read transactions take `RLock`. Write transactions take `Lock`.
- Awareness has its own `sync.RWMutex`.
- Public API is goroutine-safe. Internal mutex never exposed.

## Persistence

```go
type Store interface {
    LoadState(ctx context.Context, docID string) ([]byte, error)
    SaveUpdate(ctx context.Context, docID string, update []byte) error
    LoadStateVector(ctx context.Context, docID string) ([]byte, error)
    Compact(ctx context.Context, docID string) error
}
```

Reference impl: `pkg/store/sqlite` using `modernc.org/sqlite`.

## Performance

- No `interface{}`/`any` in hot paths. Tagged-union struct with `Kind` field.
- Varint encode/decode: `[]byte` only, no `io.Reader`/`io.Writer`.
- Single `sync.RWMutex` per Doc.
- Goal: within 2x of yrs on `dmonad/crdt-benchmarks` B1-B4 by v1.0.

## Module layout

```
github.com/Deln0r/ygo/
  ygo.go                  # public API surface: Doc/Map/Array/Text/Xml*/Awareness aliases + helpers
  ygo_test.go             # external-API smoke tests (package ygo_test)
  doc.go                  # package docstring + Version constant
  persist/                # Store interface (public)
  persist/sqlite/         # modernc.org/sqlite reference impl (public)
  server/                 # WebSocket sync server (public; http.Handler)
  cmd/ygo-server/         # stand-alone server binary
  gomobile/               # bytes-only subset for `gomobile bind` (iOS/Android)
  internal/lib0/          # varint primitives
  internal/block/         # Item / Branch / TypeRef / Repair / Integrate
  internal/store/         # BlockStore
  internal/doc/           # Doc / Transaction / TransactionMut
  internal/encoding/      # V1 update + state-vector + IdSet + pending buffer
  internal/awareness/     # y-protocols Awareness layer
  internal/sync/          # y-sync + Hocuspocus message framing + handler
  internal/types/         # Map / Array / Text (plain + rich) / Xml*
  internal/utf16/         # UTF-16 length / SplitAt with U+FFFD
  docs/                   # design + tech-debt + yrs port notes
  testdata/               # cross-language fixtures (V1 update, lib0, ...)
  testdata/gen/           # Node.js fixture generator
  benchmarks/             # dmonad/crdt-benchmarks port (planned)
```

External Go code only needs:

```go
import "github.com/Deln0r/ygo"             // Doc, Map, Array, Text, Xml*, Awareness
import "github.com/Deln0r/ygo/persist/sqlite"   // sqlite Store impl
import "github.com/Deln0r/ygo/server"      // WebSocket sync server
```

The `internal/*` packages are deliberately not importable from outside the module.

## Working materials

- `docs/yjs-architecture-notes.md` — distilled Yjs/Yrs reference, organized by concept, citing Sypytkowski / INTERNALS / docs.yjs.dev for every non-obvious claim. Read this before touching any code.
- `docs/yrs-port-notes/` — per-layer summaries of the Rust `yrs` source, generated incrementally as each layer is ported. Each file (`block.md`, `store.md`, `transaction.md`, `types.md`, `update.md`, `protocol.md`) is the durable working memory used while implementing that layer; written before the Go skeleton, refined as integration questions surface.

## References

- [Yjs](https://github.com/yjs/yjs) — original JS implementation
- [yrs](https://github.com/y-crdt/y-crdt) — Rust port (executable spec for ygo)
- [Bartosz Sypytkowski: Deep dive into Yrs architecture](https://www.bartoszsypytkowski.com/yrs-architecture/)
- [yjs/yjs INTERNALS.md](https://github.com/yjs/yjs/blob/main/INTERNALS.md)
- [docs.yjs.dev](https://docs.yjs.dev/)
