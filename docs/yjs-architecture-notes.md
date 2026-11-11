# Yjs / Yrs Architecture Reference

> Source synthesis for a pure-Go port. Citations use `[Sypytkowski]` for the Yrs deep-dive (bartoszsypytkowski.com/yrs-architecture/), `[INTERNALS]` for yjs/yjs `INTERNALS.md`, and `[docs.yjs.dev]` for the public API docs. Where a topic is not covered by any fetched source, it is marked **NOT-IN-SOURCES** and deferred to direct reading of the `yrs` (Rust) and `yjs` (JS) source.

---

## 1. Mental model

Yjs and Yrs are **delta-state CRDTs** built around a single underlying primitive: a **list CRDT**. All higher shared types are projections of that list:

- **Y.Array** is the list directly.
- **Y.Text** is a list of characters (with optional formatting markers and embeds inserted as list elements) [INTERNALS].
- **Y.Map** is a list of `(key, value)` insertions where, per key, only the *last* insertion is observable; older insertions for the same key are flagged deleted [INTERNALS].
- **Y.XmlFragment / Y.XmlElement / Y.XmlText** are XML projections built on top of the same list/map machinery [docs.yjs.dev].

The list CRDT used is **YATA** (Yet Another Transformation Approach). Conflict-free convergence is achieved by deterministically ordering concurrent inserts via the IDs of their immutable left/right *origin* neighbours, not by post-hoc operational transformation [INTERNALS].

---

## 2. ID structure

```text
ID = (clientID: u64-ish, clock: u32-ish)
```

- **clientID**: a random per-session integer. yjs 13 and 14 draw it as a uint32 (`generateNewClientId = random.uint32`), and switch to a new one when a remote transaction advances their own (`cleanupTransactions`). On the wire it is a varint, and any value that fits 53 bits (the JS safe-integer range) round-trips. **Porting trap**: generated IDs are 32-bit, decoded ones are not: do not truncate decoded IDs to 32 bits.
- **clock**: a Lamport-style counter, **incremented per inserted element**, not per Item [INTERNALS]. A single Item that contains the 3-character string `"abc"` occupies clock range `[c, c+3)`; the next insertion gets clock `c+3` [Sypytkowski].
- **Deletions do not advance clock** [INTERNALS]. They are handled state-based via the delete set (see §8).
- Clock starts at 0 per (doc, clientID) pair on the first insert [INTERNALS].

The pair `(clientID, clock)` uniquely names every inserted element across the system; an Item that spans `len` elements implicitly owns IDs `(clientID, clock) … (clientID, clock+len-1)` [Sypytkowski]. This is essential for **block splitting**: when a remote operation references a clock that lands in the middle of a local Item, the Item is split into two smaller Items, each retaining its share of the ID range [Sypytkowski].

---

## 3. Item / Block layout

Yjs calls these `Item` (`src/structs/Item.js`); Yrs calls them `Block` [INTERNALS][Sypytkowski]. They are the same thing.

Logical fields:

```text
Item {
    id:          ID                 // (clientID, clock) of first element in this item
    len:         u32                // number of elements (derived from content)
    origin:      Option<ID>         // ID of the element that was the LEFT neighbour at insert time (immutable)
    rightOrigin: Option<ID>         // ID of the element that was the RIGHT neighbour at insert time (immutable)
    left:        Option<*Item>      // current left neighbour in the doubly-linked list (mutable, traversal)
    right:       Option<*Item>      // current right neighbour (mutable, traversal)
    parent:      ParentRef          // containing branch (root type, or another Item whose content is a nested type)
    parentSub:   Option<string>     // for map-like parents: the key this insertion is under
    info:        bitfield           // flags incl. tombstone / deleted, content type tag
    content:     Content            // see §5
}
```

Source quotes:

- "`origin`: ID of preceding item ... `originRight`: ID of succeeding item (performance optimization for concurrent inserts)" [INTERNALS].
- "`info`: Bitfield (deletion flagging)" [INTERNALS].
- Yrs blocks carry: unique id, left/right origins (immutable, used by YATA), left/right pointers (mutable, traversal), parent, key (for map-like), bit-flag field, content [Sypytkowski].

### 3.1 Why `origin` *and* `left` (and `rightOrigin` *and* `right`)

- The **origin pair** is the immutable insertion context — the IDs of whatever sat to the left/right of the cursor when this item was first produced. YATA needs this forever to resolve conflicts deterministically.
- The **left/right pointers** are the live doubly-linked-list neighbours at the current moment, used for O(1) traversal and rendering. They change as new items are integrated between this one and its neighbours.

`rightOrigin` is explicitly called out as a Yjs optimization not in the original YATA paper: it lets the integration loop short-circuit when "many concurrent inserts happen after the same character" [INTERNALS]. **Porting note**: keep both — skipping `rightOrigin` will produce a correct but pathologically slow implementation under heavy concurrent typing.

### 3.2 Item-per-run optimization

Consecutive single-character inserts by the same client at adjacent clocks and adjacent positions are **collapsed into a single Item** holding a string content of length N rather than N separate Items [INTERNALS]. The Item is split lazily if anything later interrupts the run (e.g. a remote insert lands in the middle, or a deletion bisects it) [INTERNALS][Sypytkowski].

"The optimization dramatically decreases the number of javascript objects created" [INTERNALS]. **Porting trap**: design `Content` as a slice/array, not a single value, and design the splitter early. Do not start with one-element-per-Item and "optimize later" — the integration algorithm assumes the splitter exists.

### 3.3 Bitfield (`info`)

Confirmed bits:

- `deleted` (tombstone) [INTERNALS][Sypytkowski].
- Content-type tag (which `Content` variant) [Sypytkowski].

**NOT-IN-SOURCES**: exact bit positions, the "keep" bit Yjs uses for snapshots, and the "countable" bit. Read `src/structs/Item.js` constants directly.

---

## 4. Block store / StructStore

- All Items produced by a given client are stored **per-client, in clock order**, in a list that supports **binary search by clock** [INTERNALS]. Yjs calls this `StructStore` (`src/utils/StructStore.js`); Yrs calls it the block store [Sypytkowski].
- "All blocks produced by the same client are laid out next to each other in memory" [Sypytkowski].
- Two parallel views of the same data exist [INTERNALS]:
  1. The per-client, clock-ordered store — used for ID lookups and for computing what to send during sync (gap detection).
  2. The doubly-linked list in document order, threaded through `left`/`right` — used for reads and rendering.

### 4.1 Position lookup cache (skip list / search marker)

Local edits arrive as document-position offsets and must be resolved to an Item + intra-Item offset. A naive walk is O(n). Yjs maintains an **80-entry skip-list-style cache of recently used positions** keyed by absolute index → Item ref, updated heuristically when "a new position significantly diverges from existing markers" [INTERNALS]. **Porting note**: implement as `[]searchMarker` of bounded size, LRU-ish; correctness does not depend on it but performance on long documents does.

---

## 5. Content variants

Yrs/Yjs encode the payload of an Item via a tagged union. Variants explicitly mentioned across sources:

- **String** — runs of characters, used by Y.Text and as Map values. Splittable per character [Sypytkowski].
- **Embedded plain object** — JSON-ish primitive (number, string, boolean, plain object) stored verbatim as a single non-CRDT value [Sypytkowski].
- **Type / nested CRDT** — the content *is* another shared type instance (the `_item.content` back-reference) [INTERNALS]; this is how nesting works (`Y.Map` inside `Y.Array`, etc.).
- **GC** (`src/structs/GC.js`) — a "very lightweight structure" that replaces the content of a deleted Item once the document has fully observed the deletion, storing only the removed length [INTERNALS]. It still occupies the ID range so that incoming references to those IDs remain valid.
- **Skip** — referenced by Yjs as a struct kind alongside `Item` and `GC` for representing gaps. **NOT-IN-SOURCES** in detail; see `src/structs/Skip.js`.

**NOT-IN-SOURCES**: full enumeration with type-tag IDs (`ContentString=4`, `ContentBinary=3`, `ContentEmbed=5`, `ContentFormat=6`, `ContentType=7`, `ContentAny=8`, `ContentDoc=9`, `ContentDeleted=1`, `ContentJSON=2`, etc.). The fetched architecture article does not enumerate variants; the canonical list lives in `src/internals.js` exports and `src/structs/Content*.js`. Treat the numbers above as **read-from-source, not authoritative-from-this-doc**.

### 5.1 ContentFormat (Y.Text formatting)

Y.Text formatting (`bold`, `italic`, custom attributes) is implemented by inserting **format-marker Items** into the same list as character Items [INTERNALS, by inference from "character sequences with optional formatting markers and embeds"]. Format spans are open intervals between markers. The exact open/close semantics are **NOT-IN-SOURCES** at byte level — read `src/types/YText.js` (`formatText`, `cleanupFormattingItems`).

---

## 6. AbstractType and root types

- Every shared type extends `AbstractType` (`src/types/AbstractType.js`) [INTERNALS].
- An `AbstractType` instance owns a `_item` back-reference to the Item whose content is this type (null for root types) and a `_start` pointer to the first Item in its list [INTERNALS, by inference]. Map-like types additionally hold `_map: Map<string, Item>` pointing at the head Item per key [INTERNALS, by inference from map "entry list where last insertion per key wins; duplicates flagged deleted"].
- **Root types** are registered on the `Y.Doc` by name and class:
  - `doc.getMap(name = ''): Y.Map`
  - `doc.getArray(name = ''): Y.Array`
  - `doc.getText(name = ''): Y.Text`
  - `doc.getXmlFragment(name = ''): Y.XmlFragment`
  - `doc.get(name, TypeClass): T` — generic [docs.yjs.dev]
- Root types are identified on the wire by `(name, type-tag)`; non-root parents are identified by the parent Item's ID. **Porting trap**: V1 update format infers the parent from neighbour blocks where possible and only emits the explicit parent ref for root-anchored items [Sypytkowski].

---

## 7. State vector

```text
StateVector = map<clientID, clock>   // for each client, the next clock the local doc has NOT yet seen
```

- Compactly encoded as a `Uint8Array` using varints for both client IDs and clocks [INTERNALS].
- Used in **sync step 1**: peer A sends `SV(A)` to peer B; peer B replies with all Items whose IDs are `>= SV(A)[client]` per client [INTERNALS]. This is the core delta-state protocol.
- Implemented in the `y-protocols` package, not in core `yjs` [INTERNALS].

**Porting trap**: the state vector is a map, not a list. Iteration order during encoding is not specified; both peers must encode their own SV but neither needs to match the other's order. Do not assume sorted-by-clientID encoding unless you read the encoder.

---

## 8. Delete set

```text
DeleteSet = map<clientID, []DeleteRange>
DeleteRange { clock: u32, len: u32 }   // [clock, clock+len) is deleted
```

- Deletions are **state-based**, not operation-based: there is no per-deletion ID, no deletion timestamp, no deleting-user record [INTERNALS]. The delete set just states "for client X, the clock ranges [a, a+l₁), [b, b+l₂), … are tombstoned".
- Encoded as **run-length pairs** per client. Real-world example from INTERNALS (B4 benchmark, 182k inserts + 77k deletes) compresses to **~4.5 KB** [INTERNALS]. Yrs uses the same `(start, length)` representation grouped by client [Sypytkowski].
- Locally, an Item's `info.deleted` flag is also set [INTERNALS]; the delete set is the canonical, transmissible form.

**Porting traps**:
- Always compose / merge overlapping ranges before encoding; the format assumes sorted, non-overlapping `(clock, len)` pairs per client.
- Snapshots use the delete set rather than the per-Item `deleted` flag to determine tombstone state at a past moment [INTERNALS] — the in-memory flag may have been GCed, but the delete set is authoritative.

---

## 9. Update format (V1 / V2)

### V1

The default wire format. Encodes:

1. The set of new Items grouped by client, in clock order.
2. The delete set.

Optimizations [Sypytkowski]:

- Block IDs are inferred from `(start clock, length)`, so per-Item IDs are not re-emitted for runs.
- Parent is inferred from the neighbour Items where possible.
- Bit flags compactly encode which optional fields (origin, rightOrigin, parent, parentSub) are present.
- Root types are inferred from the leading block's parent slot rather than emitted as explicit type names every time.
- All integers are varint-encoded.

### V2

An "Automerge-influenced" format that adds **column-oriented run-length encoding** over many fields, yielding much smaller payloads on bulk updates [Sypytkowski]. Same logical content, different physical layout. V2 has its own encoder/decoder pair.

**Porting traps**:

- V1 and V2 are **not interchangeable on the wire**. A peer must negotiate or accept both. Yjs ships separate `encodeStateAsUpdateV2` / `applyUpdateV2` functions.
- Varint encoding follows the `lib0` flavour: little-endian 7-bit groups, MSB = continuation. Do not use protobuf's zigzag for unsigned varints; do use a signed-varint variant for fields that can be negative.
- Field presence is driven by bit flags in the Item header — emit the flag byte first, then emit only the fields it indicates.

**NOT-IN-SOURCES**: the exact byte-for-byte layout (header bit positions, content-type tags, field order). Authoritative source is `src/utils/UpdateEncoder.js` / `UpdateDecoder.js` and the `lib0/encoding` module.

---

## 10. YATA conflict resolution

YATA assigns every concurrent insert a deterministic place by examining the immutable `origin` (left) and `rightOrigin` (right) IDs.

### 10.1 Integration entry point

A remote Item arrives with its origin pair set. Local integration:

1. Resolve `origin` and `rightOrigin` to local Items via the StructStore. If either is missing (some prior insert hasn't been received yet), **buffer this Item as pending** and stop. It will be retried after a future update closes the causal gap [INTERNALS, by inference from "efficient sync gap detection"].
2. If both origins are resolvable, possibly **split** the local Items containing the target clocks so that the integration boundary lies between Items, not inside one [Sypytkowski].
3. Walk forward from the Item just after `origin` (or from the parent's `_start` if `origin` is null) until reaching `rightOrigin` (or end), comparing each candidate already-integrated Item to the new one. Insert the new Item at the first position where the YATA ordering rule says "the new Item belongs here".
4. Update the doubly-linked-list neighbours' `left`/`right` pointers; if `parentSub` is set (map-like), tombstone any prior Item under that key and update `parent._map[parentSub]` to point at the new Item.

### 10.2 Ordering rule

For two items with the same `origin`, the one whose author has the **higher (clientID, clock)** wins the right-most position [Sypytkowski: "last means the block with the highest client ID and clock value"]. The rule is applied symmetrically against `rightOrigin` to bound the search window.

The Yjs implementation in `Item#integrate()` is described as "only a couple dozen lines of code" [INTERNALS] and is the single most-tested path in the codebase. **NOT-IN-SOURCES**: full pseudocode. For a Go port, transcribe directly from `src/structs/Item.js` `integrate()` — do not reconstruct from prose.

### 10.3 Tiebreaker direction

The fetched sources state the rule but differ in framing. Confirmed:

- Tiebreak is by **clientID** (then clock) of the new item vs. the conflicting one [Sypytkowski].
- The comparison is **lexicographic on clientID** — there is no notion of "last writer wins by wall-clock time"; the ordering is purely by the immutable IDs.

### 10.4 Map semantics on top of YATA

For map-like parents (Y.Map, XML attributes), a `set(key, v)` is implemented as appending a new Item with `parentSub = key`. Older Items under the same key are tombstoned. Because YATA orders concurrent appends deterministically, **two clients setting the same key concurrently both produce Items, but only the YATA-rightmost one is observable as the value** — the other is created-then-tombstoned [Sypytkowski]. The "winner" is the higher (clientID, clock).

**Porting trap**: there is no separate last-writer-wins register CRDT here. Map convergence falls out of YATA.

---

## 11. Garbage collection

- Triggered on transaction commit when `doc.gc === true` (default) [docs.yjs.dev].
- A deleted Item whose deletion is fully observed (delete set covers it across all clients) may have its `content` replaced with a `GC` struct holding only `len` [INTERNALS]. The Item's ID range remains valid so that future references resolve.
- Setting `doc.gc = false` on a Y.Doc disables this, retaining content for snapshot restoration [docs.yjs.dev].

**Porting trap**: GC is **not** "forget the Item entirely". The Item shell must remain because remote peers can still reference its IDs as `origin` / `rightOrigin` indefinitely. Also: do not GC if the Item is still referenced by an active pending update.

---

## 12. Block squashing

Adjacent Items can be merged at transaction commit if [Sypytkowski]:

A. They are immediate neighbours in the document.
B. They were emitted by the same client.
C. Their IDs are *strictly* consecutive — `id.clock + len == nextItem.id.clock`, no holes.
D. Their content types support concatenation (string-with-string, deleted-with-deleted, etc.).
E. Both have the same `deleted` state and same `parent` / `parentSub`. (Implied — sources don't list this explicitly, but it's required for correctness.)

Squashing happens on commit, not eagerly on every keystroke [Sypytkowski].

---

## 13. Transaction model

- Every mutation must occur inside a transaction. JS API: `doc.transact(fn, origin?)` [docs.yjs.dev].
- Transactions [INTERNALS][Sypytkowski]:
  - Batch a series of local mutations into a single observable change.
  - Track newly inserted Items and the IDs deleted within the transaction.
  - On commit: run squashing, run GC if eligible, fire observers, emit the `update` event with the encoded delta.
- Nested `transact` calls collapse into the outermost transaction [docs.yjs.dev, by inference from event firing semantics].
- An `origin` opaque value can be attached to a transaction so observers can distinguish e.g. local vs. remote-applied changes [docs.yjs.dev].

Doc-level events:

- `doc.on('beforeTransaction', fn)`
- `doc.on('afterTransaction', fn)`
- `doc.on('update', fn(updateBytes, origin, doc, transaction))` [docs.yjs.dev]

**Porting trap**: in Go, model the transaction as an explicit struct passed to mutation methods; do not use goroutine-local state. Yjs's reliance on a single-threaded event loop is not portable.

---

## 14. Observer events

### 14.1 Firing

- Observers fire **synchronously after a transaction commits**, not at the moment of the individual mutation [docs.yjs.dev: "synchronous listener"; INTERNALS implies post-commit batching].
- `observe(fn)` fires for direct mutations of the type. `observeDeep(fn)` fires for direct + descendant mutations and receives an **array of events** in the order they happened in the subtree [docs.yjs.dev].
- If an observer mutates the document during its callback, those mutations form a new transaction whose observers fire after the current batch completes [docs.yjs.dev: "Fires again if modifications occur within the callback"].

### 14.2 Y.Event base class [docs.yjs.dev]

```text
event.target        // shared type where the change happened
event.currentTarget // shared type whose observer is firing (differs under observeDeep)
event.transaction   // the Transaction
event.path          // []any walking from doc root to event.target; resolve via ydoc.get(path[0]).get(path[1])...
event.changes       // type-specific; see below
```

### 14.3 YArrayEvent

`event.changes.delta` is a Quill-style delta over the array [docs.yjs.dev]:

- `{ insert: [v1, v2, ...] }`
- `{ delete: N }`
- `{ retain: N }`

Example: `arr.insert(1, ['a','b']); arr.delete(2, 2)` ⇒ `[{retain:1},{insert:['a']},{delete:1}]` [docs.yjs.dev].

### 14.4 YMapEvent

```text
event.target            // the Y.Map
event.keysChanged       // Set<string>
event.changes.keys      // Map<string, { action: 'add'|'update'|'delete', oldValue: any }>
```

`oldValue` is provided for `update` and `delete` actions [docs.yjs.dev].

### 14.5 YTextEvent

`event.delta` is a Quill-compatible text delta:

```text
[ { insert: 'a' },
  { insert: 'bc', attributes: { bold: true } },
  { retain: 3, attributes: { italic: true } },
  { delete: 2 },
  { insert: { image: 'url' } }    // embeds
]
```

[docs.yjs.dev]. Detailed payload structure for `event.changes` on Y.Text is marked `[todo]` upstream [docs.yjs.dev]; treat the delta as authoritative.

---

## 15. Sync protocol

Implemented in `y-protocols` [INTERNALS]:

1. **Sync step 1**: peer A sends `encodeStateVector(docA)` to peer B.
2. **Sync step 2**: peer B replies with `encodeStateAsUpdate(docB, sv_from_A)` — the minimal set of Items + delete set that A is missing.
3. (Bidirectional symmetry: A also responds to B with what B is missing, derived from B's SV.)
4. Live updates: after each local transaction commits, the doc emits an `update` event whose payload is the V1 (or V2) bytes; clients broadcast these.

Awareness protocol (cursor positions, presence) is a **separate** protocol layered on top, not part of the CRDT [INTERNALS, by absence — and confirmed by the `y-protocols` split]. **NOT-IN-SOURCES** in fetched material.

---

## 16. Pending updates

- Updates can arrive out of causal order. Items whose `origin` or `rightOrigin` references an as-yet-unseen ID are buffered, not dropped [INTERNALS, by inference from StructStore "efficient sync gap detection"].
- After each new update is applied, the buffer is rescanned to retry pending Items.
- The state vector advances **only** for clocks that are contiguously known — a missing clock in the middle of a client's range stops SV advancement at the gap [INTERNALS, by inference].

**Porting trap**: do not advance `SV[client]` past the highest clock seen if there are gaps; advance only past the contiguous prefix. Otherwise sync step 2 will under-request and the gap will never heal.

---

## 17. Snapshots

- A snapshot = `(StateVector, DeleteSet)` [INTERNALS].
- Restoration = walk current doc, hide Items whose `id.clock >= SV[id.client]`, and treat tombstones according to the delete set rather than the live `info.deleted` flag [INTERNALS].
- Yjs explicitly warns: "It is not recommended to restore an old document state using snapshots" — they're for read-only diff/blame views, not time travel [INTERNALS].
- Snapshots require `doc.gc = false` to be reliable; otherwise GC may have replaced the content of Items that the snapshot needs to render [docs.yjs.dev: "set to false to restore old content"].

---

## 18. Shared-type API surface

(From `[docs.yjs.dev]`. Method signatures are reproduced verbatim.)

### 18.1 Y.Doc

```text
new Y.Doc()
doc.clientID                                                  // readonly number
doc.gc                                                        // boolean, default true
doc.getMap(name=''): Y.Map
doc.getArray(name=''): Y.Array
doc.getText(name=''): Y.Text                                  // by analogy; not in fetched index but standard
doc.getXmlFragment(name=''): Y.XmlFragment
doc.get(name, TypeClass): T
doc.transact(fn(Transaction), origin?)
doc.on('update' | 'beforeTransaction' | 'afterTransaction', fn)
doc.destroy()
```

### 18.2 Y.Map

```text
new Y.Map()
ymap.set(key: string, value: object|boolean|string|number|Uint8Array|Y.AbstractType): void
ymap.get(key: string): object|boolean|Array|string|number|Uint8Array|Y.AbstractType|undefined
ymap.delete(key: string): void
ymap.has(key: string): boolean
ymap.clear(): void
ymap.size                                                     // readonly number
ymap.forEach(fn(value, key, map))
ymap.entries() / keys() / values() / [Symbol.iterator]()
ymap.toJSON(): object
ymap.clone(): Y.Map
ymap.observe(fn(YMapEvent, Transaction))
ymap.observeDeep(fn(Array<Y.Event>, Transaction))
ymap.unobserve(fn) / unobserveDeep(fn)
ymap.doc / ymap.parent
```

### 18.3 Y.Array

```text
new Y.Array()
Y.Array.from(items: Array<...>): Y.Array
yarray.insert(index: number, content: Array<...>)            // content MUST be an array
yarray.push(content: Array<...>)                             // ≡ insert(length, content)
yarray.unshift(content: Array<...>)                          // ≡ insert(0, content)
yarray.delete(index: number, length: number)
yarray.get(index: number): JSON|Uint8Array|Y.AbstractType
yarray.slice(start?, end?): Array<...>                       // negative indices supported
yarray.length                                                // readonly
yarray.toArray() / toJSON() / forEach(fn) / map(fn) / [Symbol.iterator]()
yarray.clone(): Y.Array
yarray.observe(fn(YArrayEvent, Transaction))
yarray.observeDeep(fn(Array<Y.Event>, Transaction))
yarray.unobserve(fn) / unobserveDeep(fn)
yarray.doc / yarray.parent
```

### 18.4 Y.Text

```text
new Y.Text(initialContent?)
ytext.insert(index: number, content: string, format?: object)
ytext.delete(index: number, length: number)
ytext.format(index: number, length: number, format: object)
ytext.applyDelta(delta)
ytext.toString(): string
ytext.toJSON(): string
ytext.toDelta(snapshot?, prevSnapshot?, computeYChange?): Delta
ytext.length                                                 // UTF-16 code units
ytext.clone(): Y.Text
ytext.observe(fn(YTextEvent, Transaction))
ytext.observeDeep(fn(Array<Y.Event>, Transaction))
ytext.unobserve / unobserveDeep
ytext.doc / ytext.parent
```

Direct quote on length [docs.yjs.dev]:
> "The length of the string in UTF-16 code units. Since JavaScripts' String implementation uses the same character encoding `ytext.toString().length === ytext.length`."

### 18.5 Y.XmlFragment / Y.XmlElement / Y.XmlText

Container methods: `insert`, `insertAfter`, `delete`, `push`, `unshift`, `get`, `slice`, `toJSON`, `toDOM`, `createTreeWalker(filter)`, `observe`, `observeDeep`. Properties: `doc`, `parent`, `firstChild`, `length` [docs.yjs.dev]. Y.XmlText extends Y.Text [docs.yjs.dev].

---

## 19. Position semantics — the UTF-16 vs UTF-8 trap

This is the single biggest API-level decision a Go port faces.

- **JS Y.Text uses UTF-16 code units** for all `index` and `length` parameters [docs.yjs.dev, quoted above]. A 4-byte UTF-8 emoji like `"😀"` occupies **2** UTF-16 code units, and `ytext.length === 2` for it.
- The wire format itself is independent of JS's string encoding — Items hold sequences of *characters* and the splitter operates per code-unit-on-the-wire.
- A Go port has three options:

| Option | Pros | Cons |
|---|---|---|
| Mirror UTF-16 indexing exactly | Perfect API parity with JS clients via shared deltas. | Forces UTF-16 conversions on every read/write in Go where strings are UTF-8 byte slices. Awkward and slow. |
| Use UTF-8 byte indices natively | Idiomatic Go; zero-cost for ASCII. | Public API diverges from JS; deltas exchanged with JS clients must be re-indexed. |
| Use rune (code-point) indices | Unicode-correct; matches user intuition. | Diverges from BOTH JS and the wire; requires translation in two directions. |

**Porting recommendation**: pick one and document it loudly. Whatever the public Go index unit is, the **internal Item content** must still be a byte/code-unit sequence that round-trips bit-identical with the JS encoder, because two clients exchanging V1 updates compare ID ranges and content lengths and must agree on what "one character" means at the wire. The natural internal unit that round-trips with JS is **UTF-16 code units**, because that is what the JS `String` slicer in `Item.split` uses to produce sub-Items. Storing internally as UTF-16 and exposing UTF-8 byte indices at the API edge is one viable design.

This is **not optional**: a Go port that stores content as UTF-8 and computes split offsets in bytes will produce IDs that disagree with JS on every multi-byte character. Two such peers will diverge silently after the first emoji.

---

## 20. Aggregated porting traps

1. **Clock per element, not per Item.** Block IDs are non-contiguous between Items by design.
2. **Keep both `origin` and `rightOrigin`.** Skipping `rightOrigin` is correct but pathologically slow under concurrent typing.
3. **Implement Item splitting before integration.** Integration assumes mid-Item boundaries can be materialized on demand.
4. **String content must be a slice, not a value.** Single-element Items are an anti-optimization; the runtime will immediately want to merge them.
5. **Store internally in the same code-unit as JS (UTF-16) for wire compat.** §19. Translate to UTF-8 at the API boundary if desired.
6. **Delete set ranges must be sorted, merged, non-overlapping per client** before encoding.
7. **GC retains the Item shell.** Only the content collapses to a `GC` struct holding `len`. IDs remain referenceable forever.
8. **State vector advances only past contiguous prefixes.** A gap stops it.
9. **Pending updates must be buffered, not dropped.** Retry on each subsequent update arrival.
10. **Map convergence falls out of YATA.** Do not implement a separate LWW register; do `parentSub`-tagged appends and tombstone older same-key Items.
11. **V1 and V2 are distinct wire formats.** Do not lazily convert one to the other; implement both encoder/decoder pairs.
12. **Varints follow `lib0` flavour** (LEB128-style, MSB continuation). Use a signed-varint variant for fields that can be negative.
13. **Observer events fire post-commit, batched per transaction.** Do not fire eagerly per mutation.
14. **`doc.transact(fn, origin)` is the only mutation entry point.** Reject mutations outside a transaction at the API level; it makes batching/observer ordering trivial.
15. **YATA tiebreaker is on `clientID` (then clock), lexicographic.** No wall-clock involvement.
16. **`info` bitfield includes `deleted` and a content-type tag at minimum.** Other bits (`keep`, `countable`) are real; consult `src/structs/Item.js` for exact positions before encoding.

---

## 21. Topics deferred to source

The following were not covered in adequate detail by the fetched sources. For each, refer to the cited file in `yrs` (Rust, `https://github.com/y-crdt/y-crdt`) or `yjs` (JS, `https://github.com/yjs/yjs`).

- Exact bit positions in `Item.info`. → `yjs/src/structs/Item.js` constants (`BIT*`).
- Full enumeration of `Content*` variants and their integer type tags. → `yjs/src/structs/Content*.js`, `yjs/src/internals.js`.
- Byte-level layout of V1 and V2 updates. → `yjs/src/utils/UpdateEncoderV1.js`, `UpdateEncoderV2.js`, `lib0/encoding.js`.
- Full `Item#integrate()` pseudocode (the YATA loop body). → `yjs/src/structs/Item.js`.
- `Skip` struct semantics. → `yjs/src/structs/Skip.js`.
- Format-marker open/close semantics for Y.Text. → `yjs/src/types/YText.js` (`formatText`, `cleanupFormattingItems`).
- Awareness protocol, undo manager, relative positions, snapshot encoding details. → `y-protocols`, `yjs/src/utils/UndoManager.js`, `yjs/src/utils/RelativePosition.js`, `yjs/src/utils/Snapshot.js`.
- Exact YTextEvent `event.changes` shape (the fetched docs page marks this `[todo]`).

---

## 22. Source registry

- `[Sypytkowski]` — Bartosz Sypytkowski, "Deep dive into Yrs architecture", https://www.bartoszsypytkowski.com/yrs-architecture/
- `[INTERNALS]` — `yjs/yjs` repo, `INTERNALS.md`, https://github.com/yjs/yjs/blob/main/INTERNALS.md
- `[docs.yjs.dev]` — Yjs official API docs, https://docs.yjs.dev/api/shared-types and adjacent pages (Y.Doc, Y.Map, Y.Array, Y.Text, Y.XmlFragment, Y.Event)
