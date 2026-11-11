# yrs port notes: block / item layer

> Source: yrs commit `639db2038fa44d09f628a2650fd900e3b109ad1e` (main, fetched at task time), files: `yrs/src/block.rs`, `yrs/src/slice.rs`, `yrs/src/gc.rs`, `yrs/src/types/mod.rs` (TypePtr/TypeRef only), `yrs/src/branch.rs` (Branch/BranchPtr only). Note: there is no `yrs/src/item.rs` — `Item`, `ID`, `ClientID`, `ItemContent`, `ItemFlags`, `ItemPtr`, `BlockCell`, `GC`, `BlockRange` all live in `block.rs`. The file `yrs/src/ids.rs` is misleadingly named and actually contains `IdRanges` / `IdMapInner` (set/map machinery), not `ID` itself.

## Public Rust types

### `ClientID` (`block.rs:80-129`)

```rust
#[repr(transparent)]
#[derive(Copy, Clone, PartialOrd, Ord, PartialEq, Eq, Hash)]
pub struct ClientID(CID);
// CID = std::num::NonZeroU64 (default) or u32 (feature = "small-client").
// Default form stores `value | (u64::MAX << 53)` so the upper 11 bits are tag-set
// to keep the NonZeroU64 niche; .get() masks them back off.
// Yjs requires client IDs fit in 53 bits (JS-safe integer); it generates 32-bit ones.
```

### `ID` (`block.rs:162-178`)

```rust
#[derive(Debug, Copy, Clone, PartialOrd, Ord, PartialEq, Eq, Hash, Serialize, Deserialize)]
pub struct ID {
    pub client: ClientID, // unique replica identifier
    pub clock:  u32,      // monotonically increasing per-client sequence number;
                          // increments by # of countable elements in the inserted block, NOT by 1
}
```

### `BlockCell` (`block.rs:180-273`) — internal

```rust
pub(crate) enum BlockCell {
    GC(GC),           // collected tombstone — only ID range survives
    Block(Box<Item>), // live or soft-deleted item
}
```

`PartialEq` for `BlockCell` compares GC by full equality, but `Block` only by `id` (not contents).

### `GC` (`block.rs:275-298`) — internal

```rust
pub(crate) struct GC {
    pub start: u32,  // first clock (inclusive)
    pub end:   u32,  // last clock  (inclusive)
}
// Encodes with ref number BLOCK_GC_REF_NUMBER (0) and varuint length = end - start + 1.
```

### `ItemPtr` (`block.rs:307-314`)

```rust
#[repr(transparent)]
#[derive(Clone, Copy, Hash)]
pub struct ItemPtr(NonNull<Item>);
unsafe impl Send for ItemPtr {}
unsafe impl Sync for ItemPtr {}
// PartialEq compares by id() only, ignoring pointer identity.
```

A "pinned" raw pointer into the `Box<Item>` owned by the block store. Items move only by reallocation of the box, which yrs never does once allocated; the store keeps boxes in a `Vec` keyed by client → list of `BlockCell`.

### `Item` (`block.rs:1164-1210`)

```rust
#[derive(PartialEq)]
pub struct Item {
    pub(crate) id: ID,                          // ID of the FIRST update in this block
    pub(crate) len: u32,                        // # of splittable updates packed in this block
    pub(crate) left:         Option<ItemPtr>,   // resolved left neighbour (None = first in parent)
    pub(crate) right:        Option<ItemPtr>,   // resolved right neighbour (None = last; for maps: most-recent value)
    pub(crate) origin:       Option<ID>,        // ID of left at insertion-time (conflict resolution)
    pub(crate) right_origin: Option<ID>,        // ID of right at insertion-time
    pub(crate) content:      ItemContent,       // user payload, tagged union
    pub(crate) parent:       TypePtr,           // owning collection
    pub(crate) redone:       Option<ID>,        // UndoManager: ID of the item that revived this one
    pub(crate) parent_sub:   Option<Arc<str>>,  // map key, when parent is map-like
    pub(crate) moved:        Option<ItemPtr>,   // pointer to controlling Move item, if any
    pub(crate) info:         ItemFlags,         // u16 bitfield (see below)
}
```

### `ItemFlags` (`block.rs:1044-1155`)

Internal bit constants used in the `info: u16` field of `Item` (NOT the on-the-wire info byte):

```rust
const ITEM_FLAG_LINKED:    u16 = 0b0001_0000_0000; // 9th bit — weak-link target
const ITEM_FLAG_MARKED:    u16 = 0b0000_1000;      // 4th bit — "not used atm"
const ITEM_FLAG_DELETED:   u16 = 0b0000_0100;      // 3rd bit — tombstoned
const ITEM_FLAG_COUNTABLE: u16 = 0b0000_0010;      // 2nd bit — contributes to length
const ITEM_FLAG_KEEP:      u16 = 0b0000_0001;      // 1st bit — UndoManager wants to keep alive
```

### Wire-format / encoding constants (`block.rs:28-72`)

These ARE on-the-wire and must match Yjs exactly:

```rust
pub const BLOCK_GC_REF_NUMBER:           u8 = 0;
pub const BLOCK_ITEM_DELETED_REF_NUMBER: u8 = 1;
pub const BLOCK_ITEM_JSON_REF_NUMBER:    u8 = 2;
pub const BLOCK_ITEM_BINARY_REF_NUMBER:  u8 = 3;
pub const BLOCK_ITEM_STRING_REF_NUMBER:  u8 = 4;
pub const BLOCK_ITEM_EMBED_REF_NUMBER:   u8 = 5;
pub const BLOCK_ITEM_FORMAT_REF_NUMBER:  u8 = 6;
pub const BLOCK_ITEM_TYPE_REF_NUMBER:    u8 = 7;
pub const BLOCK_ITEM_ANY_REF_NUMBER:     u8 = 8;
pub const BLOCK_ITEM_DOC_REF_NUMBER:     u8 = 9;
pub const BLOCK_SKIP_REF_NUMBER:         u8 = 10;
pub const BLOCK_ITEM_MOVE_REF_NUMBER:    u8 = 11;

pub const HAS_RIGHT_ORIGIN: u8 = 0b0100_0000; // bit 6
pub const HAS_ORIGIN:       u8 = 0b1000_0000; // bit 7
pub const HAS_PARENT_SUB:   u8 = 0b0010_0000; // bit 5
```

The `info()` byte returned by `Item::info()` is built as:

```rust
(origin.is_some()       ? HAS_ORIGIN       : 0)
| (right_origin.is_some() ? HAS_RIGHT_ORIGIN : 0)
| (parent_sub.is_some()   ? HAS_PARENT_SUB   : 0)
| (content.get_ref_number() & 0b1111)
```

So the low nibble carries the content-type ref number (0..11); bits 5-7 carry origin/parent-sub presence; bit 4 (`0b0001_0000`) is currently unused on the wire.

### `ItemContent` (`block.rs:1594-1630`)

```rust
pub enum ItemContent {
    Any(Vec<Any>),                 // splittable, countable
    Binary(Vec<u8>),               // single element, countable
    Deleted(u32),                  // length only — NOT countable
    Doc(Option<Doc>, Doc),         // (parent_doc, child_doc) — countable, single
    JSON(Vec<String>),             // legacy stringified JSON, splittable, countable
    Embed(Any),                    // single, countable
    Format(Arc<str>, Box<Any>),    // (key, value) attribute — NOT countable
    String(SplittableString),      // splittable by UTF-16 offset, countable
    Type(Box<Branch>),             // nested complex type, countable, single
    Move(Box<Move>),               // move marker — NOT countable
}
```

`get_ref_number()` maps each variant to the wire constant; `is_countable()` returns true for everything except `Deleted`, `Format`, `Move`. `len(OffsetKind)` returns 1 for all single-element variants, the `Vec`/`String` length for splittable variants, and the stored count for `Deleted`.

### `BlockRange` (`block.rs:1213-1277`)

```rust
pub struct BlockRange {
    pub id:  ID,    // first ID of the range
    pub len: u32,   // number of clocks in range
}
```

Used for Skip blocks and as a building block for GC and the delete set. `last_id()` returns `ID(client, clock + len)` — note the off-by-one vs `Item::last_id()` which uses `clock + len - 1`. **This is a wart in the source.**

### `BlockSlice` / `ItemSlice` / `GCSlice` (`slice.rs`)

```rust
pub(crate) enum BlockSlice { Item(ItemSlice), GC(GCSlice) }

pub(crate) struct ItemSlice {
    pub ptr:   ItemPtr,
    pub start: u32,  // inclusive offset into ptr.id.clock..ptr.id.clock+ptr.len
    pub end:   u32,  // inclusive
}

pub(crate) struct GCSlice {
    pub start: u32,
    pub end:   u32,
}
```

A `BlockSlice` is a non-destructive sub-range view over a stored block. The store provides `materialize(slice)` to splice the underlying block to actually start/end at the slice boundaries.

### `ItemPosition` (`block.rs:996-1042`) — internal

```rust
pub(crate) struct ItemPosition {
    pub parent:         TypePtr,
    pub left:           Option<ItemPtr>,
    pub right:          Option<ItemPtr>,
    pub index:          u32,             // logical index within parent
    pub current_attrs:  Option<Box<Attrs>>,
}
```

Helper used by text/array insert APIs to describe where a new item should land before integration.

### Forward-referenced types (defined in `branch.rs` / `types/mod.rs`)

```rust
// types/mod.rs
pub(crate) enum TypePtr {
    Unknown,             // pre-resolution sentinel
    Branch(BranchPtr),   // resolved live branch
    Named(Arc<str>),     // root type by name (resolves to Branch on integration)
    ID(ID),              // nested type by parent item ID (resolves to Branch on integration)
}

// branch.rs
#[repr(transparent)]
pub struct BranchPtr(NonNull<Branch>);

pub struct Branch {
    pub(crate) start:       Option<ItemPtr>,           // head of indexed sequence
    pub(crate) map:         HashMap<Arc<str>, ItemPtr>,// key -> last write item
    pub(crate) item:        Option<ItemPtr>,           // None = root, else the Item that owns this branch
    pub(crate) name:        Option<Arc<str>>,          // root types only
    pub          block_len:  u32,                       // sum of len for non-deleted countable items
    pub          content_len:u32,                       // length in user-facing units (utf-16, etc.)
    pub(crate) type_ref:    TypeRef,
    pub(crate) observers:      Observer<ObserveFn>,
    pub(crate) deep_observers: Observer<DeepObserveFn>,
}
```

## Internal invariants

These are assumed by `integrate`, `splice`, `try_squash`, `repair`, and the GC pass. Every Go test we write should be able to assert them after every transaction.

1. **Doubly-linked consistency.** When both sides exist: `item.left.right == Some(item)` and `item.right.left == Some(item)`. `splice` and `integrate` always restore this (`block.rs:543-545, 697-719`).
2. **Adjacency of clocks within a client.** A block of `id=(c, k)` with `len=n` covers clocks `[k, k+n)`. The next block from client `c` (if any) starts at `k+n`. `try_squash` checks `self.id.clock + self.len() == other.id.clock` exactly (`block.rs:854`).
3. **Origin == predecessor's last id when adjacent.** After `splice(offset)`, the new right half has `origin = Some(ID::new(client, clock + offset - 1))` (`block.rs:532`). `try_squash` requires `other.origin == Some(self.last_id())` (`block.rs:855`).
4. **`origin`/`right_origin` are immutable.** They reflect the insertion-time neighbours and are part of the wire identity. `left`/`right` are local resolutions and may be re-pointed during integration / squash / GC of neighbours.
5. **`Item::len` is always UTF-16-units.** `Item::new` stamps `len = content.len(OffsetKind::Utf16)` (`block.rs:1307`). The on-the-wire length always matches Yjs UTF-16 semantics regardless of the doc's `OffsetKind`.
6. **Empty content is rejected.** `Item::new` returns `None` when `content.len(Utf16) == 0` (`block.rs:1308`).
7. **`parent` on a stored item is always `Branch(BranchPtr)`.** `integrate` resolves `Named` and `ID` variants into `Branch`; `repair` does the same when decoding updates. `Unknown` only appears transiently and aborts `integrate` returning `true` (drop).
8. **`parent_sub` propagation.** During integrate, if the new item has no `parent_sub` but its left or right neighbour does, it inherits theirs (`block.rs:686-694`).
9. **Map-entry rule.** For map-like parents (i.e. `parent_sub.is_some()`), the entry pointed to by `parent.map[key]` is always the rightmost item of the chain for that key (the most recent write); when integrating a new item that ends up with `right.is_none()` and a `parent_sub`, the previous map-entry winner is `txn.delete()`-ed (`block.rs:720-740`).
10. **Countable contribution to parent length only when not deleted and not in map slot.** `parent.block_len += this.len; parent.content_len += content_len(encoding)` only if `parent_sub.is_none() && !is_deleted() && is_countable()` (`block.rs:744-749`).
11. **GC preserves ID range.** Garbage-collecting a deleted item replaces the `BlockCell::Block(item)` with `BlockCell::GC(start..=end)` covering the same clocks (`gc.rs:collect_marked`). Other items that point to this ID range via `origin`/`right_origin` continue to resolve.
12. **GC is gated on `info.is_keep() == false`.** Items pinned by an UndoManager (`ITEM_FLAG_KEEP`) are never GC'd even if deleted (`gc.rs:collect_marked`, `block.rs:1460`).
13. **Inserts under a deleted parent are auto-deleted.** `integrate` returns `true` (caller deletes) if `parent_deleted || (parent_sub.is_some() && right.is_some())` (`block.rs:837-842`).

## Edge cases the source handles explicitly

- **Splitting an Item mid-clock-range** (`ItemPtr::splice`, `block.rs:516-560`): produces a new right half with `origin = Some(ID(client, clock + offset - 1))`, copies `right_origin`, `parent`, `parent_sub`, `moved`, `info`, and rewrites the map slot if `right.is_none() && parent_sub.is_some()`. Returns `None` for `offset == 0`.
- **GC replacing content while keeping ID range alive** (`Item::gc`, `block.rs:1459-1470`): if `parent_gc` is true the whole cell becomes `BlockCell::GC`; otherwise content is replaced with `ItemContent::Deleted(len)` and the countable flag is cleared. Recursive: `ItemContent::gc` walks `Branch` children for `Type` content.
- **`integrate` when `offset > 0`** (`block.rs:569-583`): used during update application when only the suffix of an incoming block is unseen. Resets `id.clock`, re-resolves `left`, recomputes `origin` from the new left's `last_id()`, splices `content`, decrements `len`. Comment explicitly notes this path *always* uses UTF-16 offsets regardless of the doc's `offset_kind`.
- **Conflict resolution algorithm** (`block.rs:619-684`): the Yjs concurrent-insert algorithm. Tracks `conflicting_items` and `items_before_origin` HashSets while walking right from the candidate position; ties broken by `client < client`. This is the load-bearing CRDT routine — port it byte-for-byte before writing your own version.
- **Origin/right-origin not resolvable locally** (`Item::repair`, `block.rs:1368-1431`): yrs decodes the *whole* update first then resolves; if at repair time the parent is `Unknown` it walks left/right neighbours to inherit a parent. If `TypePtr::ID(id)` resolves to a non-`Type`/non-`Deleted` content, returns `Err(UpdateError::InvalidParent)`.
- **Squash refusal cases** (`try_squash`, `block.rs:852-876`): different client, non-adjacent clocks, mismatched `right_origin`, `right != Some(other)`, mismatched deleted state, either has `redone`, either is `LINKED`, mismatched `moved`, or `content.try_squash` returns false. All must hold to merge.
- **`Branch.map` lookup walks left.** When integrating with no `left`, the parent's map entry for `parent_sub` is followed left until `left.is_none()` to find the head of the conflict chain (`block.rs:625-636, 700-710`).
- **`ItemContent::Move` integration recursively re-integrates left- and right-moved ranges** when `left.moved != right.moved` (`block.rs:760-784`).
- **Subdoc setup at integrate time** (`block.rs:792-803`): `ItemContent::Doc` writes the parent doc reference into the child store, registers the child in `txn.subdocs.added`, and conditionally in `loaded`.
- **Splittable string offset semantics** (`SplittableString::block_offset`, `block.rs:1504-1522`): bytes-encoded offsets are converted to UTF-16 by walking characters. The wire always speaks UTF-16.
- **Surrogate-pair split TODO** (`block.rs:1940-1948`): yrs has commented-out code for replacing split surrogate halves with U+FFFD. Currently a no-op. **Yjs does perform this replacement** — port it from yjs, not yrs.
- **`try_squash` TODO** (`block.rs:1973`): "change `other` to Self (not ref) and return type to Option<Self>". Cosmetic, not a correctness issue.
- **Rich-text searchmarker FIXME** (`block.rs:805-806`): `ItemContent::Format` integration leaves a comment that searchmarker reset is unimplemented in yrs. We can ignore for the first pass.

## Concrete Go translation choices

### `ItemContent` tagged union

Confirmed: the DESIGN.md choice (single struct with `Kind` discriminator + variant-specific slice/scalar fields) covers all 10 yrs variants. Concretely:

```go
type ContentKind uint8

const (
    ContentDeleted ContentKind = 1
    ContentJSON    ContentKind = 2
    ContentBinary  ContentKind = 3
    ContentString  ContentKind = 4
    ContentEmbed   ContentKind = 5
    ContentFormat  ContentKind = 6
    ContentType    ContentKind = 7
    ContentAny     ContentKind = 8
    ContentDoc     ContentKind = 9
    ContentMove    ContentKind = 11
)

type Content struct {
    Kind       ContentKind
    Anys       []Any        // ContentAny, ContentJSON (as Any), ContentEmbed (1 elem), ContentFormat value
    Bytes      []byte       // ContentBinary
    Str        string       // ContentString — store as UTF-16 code units? See note below.
    DeletedLen uint32       // ContentDeleted
    FormatKey  string       // ContentFormat
    Branch     *Branch      // ContentType — forward dependency
    Move       *Move        // ContentMove  — forward dependency
    Doc        *Doc         // ContentDoc   — forward dependency
    ParentDoc  *Doc         // ContentDoc parent reference (set at integrate time)
}
```

Use a single struct (not an interface) so `Content` is value-copyable, embeddable in `Item` without an extra allocation, and inspectable in tests without a type-switch. The wire ref-numbers are the same as the kind constants, so `(c.Kind & 0b1111)` produces the low nibble of the info byte directly.

For `ContentString` we should store **a representation that lets us slice by UTF-16 offset cheaply** because all wire offsets are UTF-16. Either store `[]uint16` (memory cost, conversion on every read) or store `string` plus a precomputed `[]uint16Index → byteIndex` table for blocks > N. Defer the decision to first benchmark; correctness-wise either works.

### Nullable `Option<ID>`

Use `*ID` (pointer to a value type). Reasons:

- `ID` is `{ClientID, uint32}` — 12 bytes; pointer is 8 bytes plus an allocation. Acceptable because `origin`/`right_origin` are set once and rarely allocated on hot paths (mostly lifted from neighbours during `Splice`).
- Sentinel-value approach (`ID{Client: 0}`) doesn't work because client `0` is a legal yrs ID (clients are arbitrary `uint64`).
- Separate `OriginValid bool` adds a field per item with marginal benefit.

For `Option<ItemPtr>` use `*Item` directly since `ItemPtr` already wraps a non-null pointer in Rust and the absence is meaningful.

### `BranchPtr` / parent reference

**Forward dependency on the types/branches layer**, which we have not yet ported. For now the Go `Item.Parent` should be a tagged-union mirror of `TypePtr`:

```go
type ParentKind uint8
const (
    ParentUnknown ParentKind = iota
    ParentBranch
    ParentNamed
    ParentID
)
type Parent struct {
    Kind   ParentKind
    Branch *Branch  // nil until branches are ported
    Named  string   // root type name
    ID     ID       // nested type identifier
}
```

When the types layer lands, `Branch` becomes a real struct and `Branch.Item *Item` provides the equivalent of `BranchPtr.item`. The `BranchPtr` raw-pointer trick is purely a Rust borrow-checker workaround; in Go we just use `*Branch`.

### `info` bitfield bit positions

Copy verbatim from `block.rs:64-72` and `block.rs:1044-1057` (already extracted above). Concretely in Go:

```go
// Wire info byte (encoded with each item)
const (
    InfoHasRightOrigin uint8 = 0b0100_0000
    InfoHasOrigin      uint8 = 0b1000_0000
    InfoHasParentSub   uint8 = 0b0010_0000
    InfoContentMask    uint8 = 0b0000_1111
)

// Internal item flags (uint16, NOT serialized)
const (
    ItemFlagKeep      uint16 = 0b0000_0000_0000_0001
    ItemFlagCountable uint16 = 0b0000_0000_0000_0010
    ItemFlagDeleted   uint16 = 0b0000_0000_0000_0100
    ItemFlagMarked    uint16 = 0b0000_0000_0000_1000  // unused in yrs
    ItemFlagLinked    uint16 = 0b0000_0001_0000_0000  // weak-link feature
)
```

`ItemFlagMarked` is dead in yrs. Keep the constant for wire-format equivalence/future-proofing but do not gate logic on it.

### `Item::splice(at)` Go signature

Rust signature: `fn splice(&mut self, offset: u32, encoding: OffsetKind) -> Option<Box<Item>>`.

Returns `None` for `offset == 0`, otherwise mutates `self` in place (truncates `len`, splits `content`) and returns the newly-allocated right half (already linked into the doubly-linked list).

Go signature:

```go
// Splice cuts item at offset and returns the new right half.
// Returns nil if offset == 0 or offset >= item.Len.
// The right half is fully linked: item.Right == new, new.Left == item,
// and the previous item.Right.Left is updated to point to new.
// The store still needs to register the returned item under its client.
func (it *Item) Splice(offset uint32, kind OffsetKind) *Item
```

Note that the Rust version takes `&mut self` on `ItemPtr` (which derefs to `&mut Item`) — we don't need an `ItemPtr` in Go, just `*Item`. The store-registration step (`store.blocks.push_block`) is the *caller's* job in yrs too.

### Concurrency

yrs uses `&mut TransactionMut` everywhere, and the doc holds a single internal `RwLock` inside `Doc::transact_mut()` — so all block manipulation is single-threaded inside a transaction. `ItemPtr` is `unsafe impl Send + Sync` only because the surrounding transaction lock makes mutation through it safe; it would be unsound under any kind of concurrent access.

In Go we hold one `sync.RWMutex` per `Doc`. All mutations go through `Doc.Transact(func(*Transaction))` (write lock) or `Doc.View(func(*Transaction))` (read lock). Implications:

- `*Item` pointers handed out by the store are valid only for the lifetime of the enclosing transaction. We should **not** expose `*Item` as part of any public API. Public APIs return values or opaque handles indexed by ID.
- The `unsafe impl Send for ItemPtr` to Go `*Item` mapping is automatic — Go pointers are safe to pass around but reading/writing through them outside a transaction is UB-equivalent for our invariants. Document this.
- `BranchPtr` / nested `*Branch` must never escape the transaction either.
- We do **not** need to port the `NonNull<Item>` / pin discipline; Go pointers don't move and don't need `NonNull` niches. Boxing in a slice (`[]Item` vs `[]*Item`) does matter for pointer stability — store as `[]*Item` so reslicing the per-client list doesn't relocate items.
- yrs's `redo`/`integrate` mutate `Branch.map` and per-item links during traversal. Under our RWMutex we get this for free (single-writer-multiple-reader); just ensure all traversals that mutate live under `Doc.Transact`.

The one real difference: yrs's `Branch.observers` are `Send + Sync` boxed closures. In Go, observer callbacks should be invoked **after** the transaction commits and the lock is released, with a snapshot of the change set, so that a callback that calls back into the doc cannot deadlock.

## Forward dependencies

This layer references but does not own:

- **`branch.rs` (`Branch`, `BranchPtr`)** — required for `ItemContent::Type` and for `TypePtr::Branch`. Stub in Go as `*Branch` until the types layer is ported.
- **`types/mod.rs` (`TypeRef`, `Attrs`, type-specific encoders)** — `ItemContent::Type` carries a `TypeRef` that gets encoded; `Format` writes attribute keys; `text::update_current_attributes` is invoked from `ItemPosition::forward`.
- **`store.rs` (`Store`, `BlockStore`, `Store::materialize`, `Store::get_or_create_type`, `Store::blocks`)** — `integrate`, `repair`, `redo`, `splice` all reach into the store. The store owns the per-client `Vec<BlockCell>` and provides `find_pivot(clock)`, `get_item(id)`, `get_item_clean_start(id)`, `get_item_clean_end(id)`, `push_block(item)`. Port outline before items are useful.
- **`transaction.rs` (`TransactionMut`)** — provides `delete_set`, `merge_blocks`, `subdocs`, `add_changed_type`, `delete(item)`. The integration logic is inseparable from these.
- **`updates/{encoder,decoder}.rs` (`Encoder`, `Decoder`, `Encode`, `Decode`)** — `ItemContent::encode/decode`, `Item::encode`, `BlockSlice::encode` all delegate. We've shipped lib0 primitives; the next step is the encoder trait.
- **`id_set.rs` (`IdSet`)** — `delete_set` and undo machinery. Touched but not extended here.
- **`gc.rs` (`GCCollector`)** — fully shown above. Tiny, port together with the block layer.
- **`moving.rs` (`Move`)** — referenced from `ItemContent::Move` and integrated during `integrate_block`. Defer until after baseline text/array works.
- **`undo.rs` (`UndoStack`)** — only `redo` references it; defer.
- **`doc.rs` (`Doc`, `DocAddr`, `OffsetKind`, `Options`)** — `OffsetKind` is needed immediately (it's a parameter to `splice`, `content_len`, `len`); `Doc` is only needed when porting subdoc support.
- **`any.rs` (`Any`)** — used inside `ItemContent::Any`, `Embed`, `Format` value, `Doc` options. Defines the JSON-like value type. Port before content does anything useful.

## Open questions

1. **`BlockRange::last_id` off-by-one.** `BlockRange::last_id() = ID(client, clock + len)` while `Item::last_id() = ID(client, clock + len - 1)`. The `BlockRange` form is used in encoded skip blocks and the delete set. Need to verify against Yjs whether the wire form is inclusive `clock + len - 1` or exclusive `clock + len` for skip lengths — pretty sure yrs's `BlockRange::last_id` is just wrong in name and only used in places that don't care, but flag it for the implementer.
2. **Surrogate-pair split.** yrs has a commented-out block (`block.rs:1940-1948`) that *would* replace split halves of a UTF-16 surrogate pair with U+FFFD. Yjs does this. We should port the Yjs behaviour, not the yrs no-op, or risk producing invalid strings on text splits at surrogate boundaries.
3. **`ITEM_FLAG_MARKED` and `ITEM_FLAG_KEEP` "not used atm" comments.** The `KEEP` flag is in fact used (UndoManager via `redo`/`keep`/GC gating). The `MARKED` flag really does appear unused — search the rest of the codebase before asserting that.
4. **`small-client` feature.** When `feature = "small-client"` is on, `ClientID` is 32-bit and the `NonZeroU64` niche trick is disabled. Yjs generates 32-bit IDs and accepts any up to 53 bits on the wire. ygo stores `uint64`, accepts up to 53 bits and, since 1.39.0, generates 32-bit ones as yjs does. Can ignore the `small-client` path entirely.
5. **`info()` byte bit 4 (`0b0001_0000`).** Currently never set on the wire (the content-type ref-number nibble only goes up to 11 = `0b1011`, so bit 4 is masked out). If a future yrs/yjs version starts using bit 4 for a flag, our decoder must not silently drop it.
6. **`Item::PartialEq`.** Derived via `#[derive(PartialEq)]` on a struct that includes raw `ItemPtr` fields. `ItemPtr::PartialEq` is overridden to compare by id only, so `Item` equality also reduces to id-based equality through neighbour pointers. Document that Go `Item.Equal(other)` should compare ID + content + flags + neighbour IDs (not pointer identity), to avoid surprises in tests.
7. **`BlockCell::PartialEq` for `Block` only checks `id`.** Anywhere the codebase relies on cell equality is implicitly id-based. Worth grepping consumer sites before committing to the same Go semantics.
8. **Format-attribute integration is a stub** in yrs (`// @todo searchmarker are currently unsupported`). If we port rich text we may need to consult Yjs for the actual implementation rather than yrs.
