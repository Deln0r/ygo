# yrs port notes: Array shared type

> Source: yrs `main`, files: `yrs/src/types/array.rs`, `yrs/src/types/mod.rs` (Change/ChangeSet/event_change_set), `yrs/src/branch.rs` (Branch struct, `Branch::index_to_ptr`, `Branch::insert_at`, `Branch::remove_at`, `Branch::path`, `Branch::len`/`content_len`), `yrs/src/block_iter.rs` (the actual position-resolution machinery used by Array hot paths), `yrs/src/transaction.rs` (`TransactionMut::create_item`, `TransactionMut::delete`).

The mental model from `array.rs:21-38`: Array is "a collection used to store data in an indexed sequence structure… implemented as a double linked list, which may squash values inserted directly one after another into single list node upon transaction commit." Countable elements are individual JSON-like primitives, individual UTF-8 characters in inserted Text, or "1" for embeds/binary/nested-types. Convergence is YATA + document-id seniority — exactly what `Item::Integrate` already provides; Array adds no CRDT logic of its own. The only thing Array adds over Map is **positional indexing into `branch.start`'s linked list, with mid-block splits** when an index lands inside a squashed run.

## 1. Public Rust types

### `ArrayRef` (`array.rs:75-86`)

```rust
#[repr(transparent)]
#[derive(Debug, Clone)]
pub struct ArrayRef(BranchPtr);

impl RootRef for ArrayRef {
    fn type_ref() -> TypeRef { TypeRef::Array }
}
impl SharedRef       for ArrayRef {}
impl Array           for ArrayRef {}
impl IndexedSequence for ArrayRef {}
```

Same layout pattern as `MapRef`: a `#[repr(transparent)]` newtype around `BranchPtr`. `IndexedSequence` is the marker trait that gates `StickyIndex`/cursor APIs; we will not port it in the first commit. `DeepObservable` and `Observable<Event = ArrayEvent>` are at `array.rs:122-125`. `From<BranchPtr> for ArrayRef` (`array.rs:482-486`) is the constructor; `TryFrom<ItemPtr>` (`array.rs:127-137`) and `TryFrom<Out>` (`array.rs:139-148`) cast across types.

### `ArrayPrelim` (`array.rs:490-549`)

```rust
#[repr(transparent)]
#[derive(Debug, Clone, PartialEq, Default)]
pub struct ArrayPrelim(Vec<In>);

impl Prelim for ArrayPrelim {
    type Return = ArrayRef;

    fn into_content(self, _txn: &mut TransactionMut) -> (ItemContent, Option<Self>) {
        let inner = Branch::new(TypeRef::Array);
        (ItemContent::Type(inner), Some(self))
    }

    fn integrate(self, txn: &mut TransactionMut, inner_ref: BranchPtr) {
        let array = ArrayRef::from(inner_ref);
        for value in self.0 {
            array.push_back(txn, value);
        }
    }
}
```

Two-phase nested construction. `into_content` returns an empty `Branch{type_ref: Array}` carried by `ItemContent::Type`; `integrate` runs after the parent Item is in the store and replays each child via `push_back` (which recurses if a child is itself an `In::Array`/`In::Map`). `Deref<Target = Vec<In>>` (`array.rs:494-507`), `From<I: IntoIterator<Item: Into<In>>>` (`array.rs:525-533`), `From<ArrayPrelim> for In` (`array.rs:509-514`).

### `RangePrelim` — *internal* (`array.rs:560-586`)

```rust
#[repr(transparent)]
struct RangePrelim(Vec<Any>);

impl Prelim for RangePrelim {
    type Return = Unused;

    fn into_content(self, _txn: &mut TransactionMut) -> (ItemContent, Option<Self>) {
        (ItemContent::Any(self.0), None)
    }
    fn integrate(self, _txn: &mut TransactionMut, _inner_ref: BranchPtr) {}
}
```

**Critical**: `insert_range` packs the entire batch into ONE `ItemContent::Any(Vec<Any>)` and integrates it as a single Item whose `Len == values.len()`. There is no per-value Item allocation when all inputs are `Into<Any>` — that is the squashing-on-insert optimisation. `Return = Unused` because the caller does not get back a ref (only `insert` returns one).

### `Array` trait (`array.rs:171-422`)

Verbatim signatures for the surface we will port:

```rust
pub trait Array: AsRef<Branch> + Sized {
    fn len<T: ReadTxn>(&self, _txn: &T) -> u32 { self.as_ref().len() }                           // 173-175

    fn insert<V>(&self, txn: &mut TransactionMut, index: u32, value: V) -> V::Return             // 186-203
        where V: Prelim;

    fn insert_range<T, V>(&self, txn: &mut TransactionMut, index: u32, values: T)                // 212-221
        where T: IntoIterator<Item = V>, V: Into<Any>;

    fn push_back<V>(&self, txn: &mut TransactionMut, value: V) -> V::Return                      // 226-232
        where V: Prelim;

    fn push_front<V>(&self, txn: &mut TransactionMut, content: V) -> V::Return                   // 237-242
        where V: Prelim;

    fn remove(&self, txn: &mut TransactionMut, index: u32) {                                     // 245-247
        self.remove_range(txn, index, 1)
    }

    fn remove_range(&self, txn: &mut TransactionMut, index: u32, len: u32);                      // 253-260

    fn get<T: ReadTxn>(&self, txn: &T, index: u32) -> Option<Out>;                               // 264-271
    fn get_as<T, V>(&self, txn: &T, index: u32) -> Result<V, Error>                              // 326-335
        where T: ReadTxn, V: DeserializeOwned;

    fn move_to(&self, txn: &mut TransactionMut, source: u32, target: u32);                       // 344-363
    fn move_range_to(&self, txn: &mut TransactionMut,
        start: u32, assoc_start: Assoc, end: u32, assoc_end: Assoc, target: u32);                // 388-415

    fn iter<'a, T: ReadTxn + 'a>(&self, txn: &'a T) -> ArrayIter<&'a T, T>;                      // 419-421
}
```

| method | what it does | block-layer ops |
|---|---|---|
| `len` | returns `branch.block_len` (`branch.rs:299-301`) — pre-computed counter maintained by `Item::Integrate` and `TransactionMut::delete`. O(1). | none |
| `insert` | builds `BlockIter::new(branch)`, walks forward `index` countable elements (splitting any block that index lands inside), calls `insert_contents` which constructs an Item with `parent_sub = None`, `origin = left.last_id()`, `right_origin = right.id`, then `Item.Integrate` + `push_block`. | `BlockIter::try_forward` (uses `Store::split_block` if needed) → `Item::new` → `Item.integrate` → `BlockStore.push_block` |
| `insert_range` | packs all `V: Into<Any>` values into a single `ItemContent::Any(Vec<Any>)` via `RangePrelim` and delegates to `insert`. **One Item for the whole batch.** | same as `insert`, but the Item carries `Len == values.len()` — a squashed run that downstream readers must slice. |
| `push_back` | `insert(txn, self.len(txn), value)`. | as above |
| `push_front` | `insert(txn, 0, value)`. Optimised path inside `Branch::insert_at` (and inside `BlockIter::try_forward(0)`) skips the walk entirely. | as above |
| `remove`/`remove_range` | `BlockIter::try_forward(index)`, then `BlockIter::delete(len)` which iterates right, splitting on partial-overlap at start and end, and calls `txn.delete` on each fully-covered Item. | `Store::split_block` ×0-2, `TransactionMut::delete` ×N |
| `get` | `BlockIter::try_forward(index)`; `read_value(txn)` reads a single-element slice via `BlockIter::slice`. | Item-content read via `ItemContent::read(offset, &mut buf)` |
| `iter` | wraps a `BlockIter` and calls `slice` of length 1 on each `next()`. | as above |

`Branch::len` is `branch.block_len`, NOT a walk. The counter is maintained by `Item::Integrate` (increment when `parent_sub.is_none() && countable`) and by `TransactionMut::delete` (decrement at `transaction.rs:725-729`):

```rust
if item.parent_sub.is_none() && item.is_countable() {
    if let TypePtr::Branch(mut parent) = item.parent {
        parent.block_len   -= item.len();
        parent.content_len -= item.content_len(store.offset_kind);
    }
}
```

So `block_len` always matches the number of live (non-deleted) countable elements. `Array.Len(txn)` is O(1). Map keys (`parent_sub.is_some()`) are excluded from this counter — same Branch, distinct counter.

## 2. Position-to-Item resolution — verbatim

Position resolution is implemented in two places that do the same job at different layers:

- `Branch::index_to_ptr` (`branch.rs:390-424`) — used by `Branch::insert_at` and `Branch::remove_at`. Splits on partial-block hit. Returns `(left, right)`.
- `BlockIter::try_forward` (`block_iter.rs:105-173`) — used by the `Array` trait directly via `BlockIter::insert_contents` / `delete` / `slice`. Tracks an `index`, an intra-block `rel` offset, and a Move stack.

Both walk `branch.start` left-to-right counting countable, non-deleted block lengths. They are not redundant: `index_to_ptr` predates `BlockIter` and is still used by `Branch::insert_at`/`remove_at` (which the *trait default impls* in `array.rs` no longer call — they go through `BlockIter` — but Branch keeps them as the `IndexedSequence` lower layer).

### `Branch::index_to_ptr` — verbatim (`branch.rs:390-424`)

```rust
fn index_to_ptr(
    txn: &mut TransactionMut,
    mut ptr: Option<ItemPtr>,
    mut index: u32,
) -> (Option<ItemPtr>, Option<ItemPtr>) {
    let encoding = txn.store.offset_kind;
    while let Some(item) = ptr {
        let content_len = item.content_len(encoding);
        if !item.is_deleted() && item.is_countable() {
            if index == content_len {
                let left = ptr;
                let right = item.right.clone();
                return (left, right);
            } else if index < content_len {
                let index = if let ItemContent::String(s) = &item.content {
                    s.block_offset(index, encoding)
                } else {
                    index
                };
                let right = txn.store.blocks.split_block(item, index, encoding);
                if let Some(_) = item.moved {
                    if let Some(src) = right {
                        if let Some(&prev_dst) = txn.prev_moved.get(&item) {
                            txn.prev_moved.insert(src, prev_dst);
                        }
                    }
                }
                return (ptr, right);
            }
            index -= content_len;
        }
        ptr = item.right.clone();
    }
    (None, None)
}
```

Line-by-line:

- `encoding = txn.store.offset_kind` — controls whether `content_len` counts UTF-16 code units or UTF-8 bytes for `ItemContent::String`. For Array of non-string content this is irrelevant; for Text it determines what "index" means. We hard-code the equivalent of `OffsetKind::Utf16` in our port (matches JS Yjs).
- The while loop walks `ptr → ptr.right` skipping deleted and non-countable items. Deleted items still occupy linked-list slots (tombstones) but contribute zero to the index.
- `is_countable()` returns false for `ItemContent::Format` and `ItemContent::Move`; both occupy a list slot but are not countable.
- `index == content_len` (boundary hit): the index sits exactly at the end of `item`. Return `(item, item.right)`. **No split.**
- `index < content_len` (interior hit): split. `Store::split_block(item, index, encoding)` mutates the linked list — left half retains `item.id`, right half gets a new ID with `clock + index` and `len -= index`. Returns the new right half. **The split is mutating, so this function takes `&mut TransactionMut`.** The `moved` bookkeeping (`item.moved`, `txn.prev_moved`) only matters for the Move CRDT subsystem we are not porting yet — strip it.
- `index -= content_len` then advance to `item.right`.
- Falls off the end (`ptr = None`): index was past the array length. Returns `(None, None)`. Callers (`insert_at`, `remove_at`) treat this as out-of-range and panic.

This is the canonical position-resolution algorithm. **Cost is O(N) in the linked-list length, where N includes tombstones until they are GC'd.** No caching layer in current yrs.

### `BlockIter::try_forward` — verbatim (`block_iter.rs:105-173`)

```rust
pub fn try_forward<T: ReadTxn>(&mut self, txn: &T, mut len: u32) -> bool {
    if len == 0 && self.next_item.is_none() {
        return true;
    }

    if self.index + len > self.branch.content_len() || self.next_item.is_none() {
        return false;
    }

    let mut item = self.next_item;
    self.index += len;
    if self.rel != 0 {
        len += self.rel;
        self.rel = 0;
    }

    let encoding = txn.store().offset_kind;
    while self.can_forward(item, len) {
        if item == self.curr_move_end
            || (self.reached_end && self.curr_move_end.is_none() && self.curr_move.is_some())
        {
            item = self.curr_move; // we iterate to the right after the current condition
            self.pop(txn);
        } else if item.is_none() {
            return false;
        } else if let Some(i) = item.as_deref() {
            if i.is_countable() && !i.is_deleted() && i.moved == self.curr_move && len > 0 {
                let item_len = i.content_len(encoding);
                if item_len > len {
                    self.rel = len;
                    len = 0;
                    break;
                } else {
                    len -= item_len;
                }
            } else if let ItemContent::Move(m) = &i.content {
                if i.moved == self.curr_move {
                    if let Some(ptr) = self.curr_move {
                        self.moved_stack.push(StackItem::new(
                            self.curr_move_start,
                            self.curr_move_end,
                            ptr,
                        ));
                    }

                    let (start, end) = m.get_moved_coords(txn);
                    self.curr_move = item;
                    self.curr_move_start = start;
                    self.curr_move_end = end;
                    item = start;
                    continue;
                }
            }
        }

        if self.reached_end {
            return false;
        }

        match item.as_deref() {
            Some(i) if i.right.is_some() => item = i.right,
            _ => self.reached_end = true, //TODO: we need to ensure to iterate further if this.currMoveEnd === null
        }
    }

    self.index -= len;
    self.next_item = item;
    true
}
```

Differences vs. `index_to_ptr`:

- **Read-only**: takes `&T: ReadTxn`. It does NOT split on interior hit. Instead it remembers `self.rel = len` — the offset *into* `next_item` — and stops. The split is deferred until `insert_contents` (`block_iter.rs:434-447 split_rel`) or until `delete` actually needs to act, both of which take `&mut TransactionMut` and call `Store::get_item_clean_start` (which materialises a clean-start split via `Store.materialize`).
- Stateful cursor: `index`, `rel`, `next_item`, `reached_end`. Subsequent `try_forward(len)` calls advance from the current cursor — used by `slice` to read N elements one at a time.
- Move-aware: `curr_move`, `curr_move_start`, `curr_move_end`, `moved_stack` — when the walk crosses an `ItemContent::Move`, push current frame, jump to the moved range's source coords, walk through, pop. Strip this for v1 (no Move support).
- Bounds check at the top: `self.index + len > self.branch.content_len()` — but this uses `content_len`, which for Array is the **codepoint/UTF-16-unit count** including counted-but-different units in Text. For Array-of-primitives, `content_len == block_len`. For Text, `content_len` is the character count. We will only call this from Array, so we can substitute `branch.block_len` in the Go port.
- The `self.rel != 0` handling at line 116-119 lets a *second* `try_forward` call resume from a partial-block stop. Important for `slice`'s incremental reads, irrelevant for one-shot index resolution.

The TODO at `block_iter.rs:166` is a known gap for Move with open-ended ranges. Not relevant to v1.

**Bottom line for the port**: implement a single function `findPosition(branch, index, txn) (left, right *Item, splitNeeded bool)`. On interior hit, immediately call `BlockStore.SplitBlock(item, offset)` (we already have that primitive) and return `(item, newRight)`. We do not need the deferred-split state machine because we will not have a `BlockIter` cursor surface in v1 — every `Array.Insert/Get/Delete` is a one-shot walk.

## 3. Search markers

**yrs `main` does NOT implement search markers.** No `SearchMarker`, `MAX_SEARCH_MARKERS`, or marker-cache field on `Branch` exists in `branch.rs`, `block_iter.rs`, `array.rs`, or `types/mod.rs` (verified by exhaustive grep). Position resolution is a linear walk of `branch.start` for every `Array.get`, `Array.insert`, `Array.remove`. JS Yjs has the cache (file `src/utils/Snapshot.js` and `src/utils/SearchMarker.js`); the yrs port elected not to copy it — there is no comment explaining the choice in the source we read, and no FIXME asking for it.

Implication: `Array` operations are O(N) where N = `branch.block_len + tombstones-since-GC`. Acceptable for arrays in the low-thousands range. **For our Go port: skip search markers in the first commit. Defer to benchmarks.** When we do add them, the JS shape is: a `Branch.searchMarkers: []SearchMarker` capped at ~80 entries; each marker is `{item *Item, index uint32}`; on every position lookup, find the closest marker by `|index - target|`, walk from there; on insert/delete inside a region, shift / invalidate downstream markers. None of this is in the executable spec we are mirroring.

**Since then (November 2026):** ygo has two marker lists. `MarkerList` (`internal/block/search_marker.go`, v1.38.0) is ygo's own LRU cache for arrays, XML children and texts without format items, where the result of a lookup does not depend on the markers. `TextMarkers` (`internal/block/text_markers.go`, v1.48.0) is a step-for-step port of yjs's `_searchMarker` (`findMarker`, `markPosition`, `overwriteMarker`, `updateMarkerChanges`, the `mergeWith` repoint, `Item.marker` as `FlagMarked`), used only by a text whose root was typed after format items arrived (`Branch.LateText`): there yjs's markers are on while format markers sit in the list, and which markers exist decides where an insert goes and which markers a delete removes.

## 4. Insert — verbatim

`Array::insert` (`array.rs:186-203`):

```rust
fn insert<V>(&self, txn: &mut TransactionMut, index: u32, value: V) -> V::Return
where
    V: Prelim,
{
    let mut walker = BlockIter::new(BranchPtr::from(self.as_ref()));
    if walker.try_forward(txn, index) {
        let ptr = walker
            .insert_contents(txn, value)
            .expect("cannot insert empty value");
        if let Ok(integrated) = ptr.try_into() {
            integrated
        } else {
            panic!("Defect: unexpected integrated type")
        }
    } else {
        panic!("Index {} is outside of the range of an array", index);
    }
}
```

The real work is in `BlockIter::insert_contents` (`block_iter.rs:458-508`):

```rust
pub fn insert_contents<V: Prelim>(
    &mut self,
    txn: &mut TransactionMut,
    value: V,
) -> Option<ItemPtr> {
    self.reduce_moves(txn);
    self.split_rel(txn);
    let id = {
        let store = txn.store();
        let client_id = store.client_id;
        let clock = store.blocks.get_clock(&client_id);
        ID::new(client_id, clock)
    };
    let parent = TypePtr::Branch(self.branch);
    let right = self.right();
    let left = self.left();
    let (mut content, remainder) = value.into_content(txn);
    let inner_ref = if let ItemContent::Type(inner_ref) = &mut content {
        Some(BranchPtr::from(inner_ref))
    } else {
        None
    };
    let mut block = Item::new(
        id,
        left,
        left.map(|ptr| ptr.last_id()),
        right,
        right.map(|r| *r.id()),
        parent,
        None,                         // ← parent_sub: None (positional, not map-keyed)
        content,
    )?;
    let mut block_ptr = ItemPtr::from(&mut block);

    block_ptr.integrate(txn, 0);

    txn.store_mut().blocks.push_block(block);

    if let Some(remainder) = remainder {
        remainder.integrate(txn, inner_ref.unwrap().into())
    }

    if let Some(item) = right.as_deref() {
        self.next_item = item.right;
    } else {
        self.next_item = left;
        self.reached_end = true;
    }

    Some(block_ptr)
}
```

Line-by-line answers to the structural questions:

- **`split_rel(txn)`** (line 464) — if the cursor stopped mid-block (`rel > 0`), split the block at the rel offset *now*, before constructing the new Item. After this call, `next_item` points at the right half, `left()` returns the left half, `rel == 0`.
- **`id = ID::new(client_id, get_clock(client_id))`** (lines 465-470) — local-client next clock. Standard.
- **`parent = TypePtr::Branch(self.branch)`** — every Array Item carries the Array's BranchPtr as its parent. Same as Map.
- **`right = self.right()`, `left = self.left()`** (`block_iter.rs:55-71`) — at a clean stop, `next_item` is the item to the right of the cursor; `left = next_item.left`. At end-of-list (`reached_end`), `next_item` is the last item and `right = None`.
- **`origin = left.last_id()`** (line 483) — `last_id() = ID{client, clock + len - 1}`. Same convention as Map: origin is the *last* clock the left Item covers. Critical when left is a squashed run.
- **`right_origin = right.id`** (line 485) — Array Items DO carry a `right_origin`, unlike Map Items which set it to None. This is what makes the YATA loop converge correctly when concurrent inserts race for the same gap: both new Items have the same `(origin, right_origin)` pair and resolve by client-id seniority.
- **`parent_sub: None`** (line 487) — flags this as a positional Item. `Item::Integrate` then maintains `branch.start` and `branch.block_len` instead of `branch.map[key]`.
- **`Item::new(...)?`** returns `Option<Item>` — `None` when the prelim produced empty content. `insert_contents` propagates this and the trait `.expect("cannot insert empty value")`s on it. We replicate: empty `[]any` insert is a no-op (`RangePrelim::is_empty` short-circuits at `array.rs:218`), but a single empty value is an error.
- **`block_ptr.integrate(txn, 0)`** runs the YATA loop. We have already ported this; it handles the `parent_sub == None` branch which inserts into `branch.start`'s linked list, updates `block_len += item.len()`, and links left/right neighbours.
- **`if let Some(remainder) = remainder { remainder.integrate(txn, inner_ref.unwrap().into()) }`** is the nested-prelim recursion: when `value` was an `ArrayPrelim` or `MapPrelim`, `into_content` returned `(ItemContent::Type(branch), Some(self))`; after the parent Item is in the store, the prelim re-enters and replays its children into the freshly-integrated child Branch.

### How are multiple values in a single insert handled?

`insert_range` (`array.rs:212-221`):

```rust
fn insert_range<T, V>(&self, txn: &mut TransactionMut, index: u32, values: T)
where
    T: IntoIterator<Item = V>,
    V: Into<Any>,
{
    let prelim = RangePrelim::new(values);
    if !prelim.is_empty() {
        self.insert(txn, index, prelim);
    }
}
```

**One Item containing all values.** `RangePrelim::into_content` returns `(ItemContent::Any(Vec<Any>), None)` (`array.rs:581-583`). The resulting Item has `Len == values.len()` — the squashing-on-insert primitive. Subsequent reads/deletes that hit the middle of this run trigger a split via `Store::split_block`.

If a value is NOT `Into<Any>` (e.g. a nested `MapPrelim`), it must go through the per-value `insert` path. `ArrayPrelim::integrate` (`array.rs:543-548`) iterates and calls `push_back` per child precisely because children may be heterogeneous prelims, not all `Into<Any>`.

### Edge cases

- **Empty array, insert at 0**: `BlockIter::new` sets `next_item = branch.start = None` and `reached_end = true` (`block_iter.rs:24-38`). `try_forward(0)` short-circuits at line 106-108 because `len == 0 && next_item.is_none()` returns `true`. `insert_contents` then computes `right = None` (via `right()` returning None when `reached_end`), `left = None` (via `left()` returning `next_item == None` when `reached_end`). New Item: `origin = None, left = None, right_origin = None, right = None`. `Item.Integrate` sees `left == None` and prepends to `branch.start`.
- **Append (`index == len`)**: `try_forward(len)` walks all the way; ends with `reached_end = true` and `next_item` pointing at the last item. `right() = None`, `left() = next_item` (= last item). New Item: `origin = last.last_id(), left = last, right = None, right_origin = None`. `Item.Integrate` appends.
- **Mid-block insert (target block has `Len > 1`)**: `try_forward` stops with `rel > 0`, `next_item` = the squashed-run Item. `insert_contents` calls `split_rel(txn)` which calls `Store::get_item_clean_start({client, clock+rel})` → `materialize(slice)` → physical split. After split, `next_item` is the new right half, `left()` is the new left half. New Item slots between the split halves. **This is the same `SplitBlock` primitive we already have in `internal/store`, so the only new code is the position walk + the split-trigger condition.**

## 5. Remove / remove_range — verbatim

`Array::remove_range` (`array.rs:253-260`):

```rust
fn remove_range(&self, txn: &mut TransactionMut, index: u32, len: u32) {
    let mut walker = BlockIter::new(BranchPtr::from(self.as_ref()));
    if walker.try_forward(txn, index) {
        walker.delete(txn, len)
    } else {
        panic!("Index {} is outside of the range of an array", index);
    }
}
```

The work is `BlockIter::delete` (`block_iter.rs:300-359`):

```rust
pub fn delete(&mut self, txn: &mut TransactionMut, mut len: u32) {
    let mut item = self.next_item;
    if self.index + len > self.branch.content_len() {
        panic!("Length exceeded");
    }

    let encoding = txn.store().offset_kind;
    let mut i: &Item;
    while len > 0 {
        while let Some(block) = item.as_deref() {
            i = block;
            if !i.is_deleted()
                && i.is_countable()
                && !self.reached_end
                && len > 0
                && i.moved == self.curr_move
                && item != self.curr_move_end
            {
                if self.rel > 0 {
                    let mut id = i.id.clone();
                    id.clock += self.rel;
                    let store = txn.store_mut();
                    item = store
                        .blocks
                        .get_item_clean_start(&id)
                        .map(|s| store.materialize(s));
                    i = item.as_deref().unwrap();
                    self.rel = 0;
                }
                if len < i.content_len(encoding) {
                    let mut id = i.id.clone();
                    id.clock += len;
                    let store = txn.store_mut();
                    store
                        .blocks
                        .get_item_clean_start(&id)
                        .map(|s| store.materialize(s));
                }
                len -= i.content_len(encoding);
                txn.delete(item.unwrap());
                if i.right.is_some() {
                    item = i.right;
                } else {
                    self.reached_end = true;
                }
            } else {
                break;
            }
        }
        if len > 0 {
            self.next_item = item;
            if self.try_forward(txn, 0) {
                item = self.next_item;
            } else {
                panic!("Block iter couldn't move forward");
            }
        }
    }
    self.next_item = item;
}
```

Line-by-line:

- **`self.rel > 0` (start-of-range mid-block split)** — if the cursor stopped inside an Item, materialise a clean-start split at `id.clock + rel`. The right half becomes the new `item`. `rel = 0`.
- **`len < i.content_len` (end-of-range mid-block split)** — if the remaining delete length is less than this Item's content, split at `id.clock + len`. We don't reassign `item` because we will delete the *left* half and the right half remains in the list.
- **`txn.delete(item.unwrap())`** — calls `TransactionMut::delete` (`transaction.rs:718+`). For positional items, this:
  - sets `item.deleted = true` (or sets the `BlockCell` flag — same thing)
  - decrements `parent.block_len -= item.len()` and `parent.content_len -= item.content_len(encoding)` (`transaction.rs:725-729`)
  - inserts `(item.id, item.len)` into the transaction's `delete_set`
  - schedules an event for the parent Branch
- **`item = i.right`** — advance. Tombstones in the linked list are skipped on the next loop iteration via the inner `if !i.is_deleted()` condition (which also breaks out to `try_forward(0)` to skip past them).
- **The outer `while len > 0` + inner break + `try_forward(0)`** is how the loop skips over already-tombstoned regions inside the delete range without counting them: `try_forward(0)` advances past non-countable / deleted items until it finds something countable.

So:

- **Length parameter is in countable elements.** A squashed run of `Len = 5` counts as 5 toward `len`; deleting 3 of them splits the run into a 3-Item + 2-Item, then tombstones the 3-Item.
- **Item.Splice equivalent** — we already have `Item.Splice` and `Store.SplitBlock`; the algorithm above is built on `get_item_clean_start` + `materialize` (which is our `SplitBlock` followed by re-fetching the right half).
- **One `txn.delete` per fully-covered Item** within the range. Up to two splits (one at start if `rel > 0`, one at end if `len < remaining_item_len`).

## 6. `get(index)` — verbatim (`array.rs:264-271`)

```rust
fn get<T: ReadTxn>(&self, txn: &T, index: u32) -> Option<Out> {
    let mut walker = BlockIter::new(BranchPtr::from(self.as_ref()));
    if walker.try_forward(txn, index) {
        walker.read_value(txn)
    } else {
        None
    }
}
```

`read_value` (`block_iter.rs:449-456`):

```rust
pub(crate) fn read_value<T: ReadTxn>(&mut self, txn: &T) -> Option<Out> {
    let mut buf = [Out::default()];
    if self.slice(txn, &mut buf) != 0 {
        Some(std::mem::replace(&mut buf[0], Out::default()))
    } else {
        None
    }
}
```

`slice` reads at most `buf.len()` countable elements into `buf` by calling `item.content.read(rel as usize, &mut buf[read..])` — the same `Content.Read(offset)` primitive we have. For a `ItemContent::Any(vec)` of length 1 stopped at `rel=0`, this returns the single value. For a squashed run stopped at `rel=k`, `read(k, buf)` returns `vec[k]`.

The `Out` extraction mirrors `Map::get`'s `content.get_last()` but with an *offset*: instead of the last value, we want the value at the cursor's current intra-block position. The mapping from `ItemContent` variant to `Out` is the same table `Map.Get` uses — for the first commit we cover `Any`, deferred for `Embed`/`Type`/`Doc`/`Move` until those subsystems land.

## 7. `iter` / forEach (`array.rs:419-480`)

```rust
fn iter<'a, T: ReadTxn + 'a>(&self, txn: &'a T) -> ArrayIter<&'a T, T> {
    ArrayIter::from_ref(self.as_ref(), txn)
}

impl<B, T> Iterator for ArrayIter<B, T>
where B: Borrow<T>, T: ReadTxn,
{
    type Item = Out;

    fn next(&mut self) -> Option<Self::Item> {
        if self.inner.finished() {
            None
        } else {
            let mut buf = [Out::default(); 1];
            let txn = self.txn.borrow();
            if self.inner.slice(txn, &mut buf) != 0 {
                Some(std::mem::replace(&mut buf[0], Out::default()))
            } else {
                None
            }
        }
    }
}
```

Each `next()` reads a slice of length 1, advancing `BlockIter` by one countable element. **Deleted items are skipped inside `slice` / `try_forward` via the `!item.is_deleted()` guard** — every consumer gets a pre-filtered live-element stream. **Squashed runs are sliced**: a single Item with `Content::Any(vec[3])` produces 3 separate `next()` results, with `BlockIter.rel` advancing 0→1→2 inside that Item before stepping to `right`.

`ToJson::to_json` (`array.rs:91-107`) takes a different path: allocates `vec![Out::default(); len]` and does a single `walker.slice(txn, &mut buf)` to drain everything in one call — useful for `array.to_json(&txn)`. We can defer this; `iter`-then-collect works equivalently for v1.

## 8. `ArrayEvent` shape (`array.rs:589-637`)

```rust
pub struct ArrayEvent {
    pub(crate) current_target: BranchPtr,
    target: ArrayRef,
    change_set: UnsafeCell<Option<Box<ChangeSet<Change>>>>,
}

impl ArrayEvent {
    pub fn target(&self) -> &ArrayRef { &self.target }
    pub fn path(&self) -> Path { Branch::path(self.current_target, self.target.0) }
    pub fn delta(&self, txn: &TransactionMut) -> &[Change]   { self.changes(txn).delta.as_slice() }
    pub fn inserts(&self, txn: &TransactionMut) -> &HashSet<ID> { &self.changes(txn).added   }
    pub fn removes(&self, txn: &TransactionMut) -> &HashSet<ID> { &self.changes(txn).deleted }

    fn changes(&self, txn: &TransactionMut) -> &ChangeSet<Change> {
        let change_set = unsafe { self.change_set.get().as_mut().unwrap() };
        change_set.get_or_insert_with(|| Box::new(event_change_set(txn, self.target.0.start)))
    }
}
```

`Change` (`types/mod.rs:776-791`):

```rust
pub enum Change {
    Added(Vec<Out>),
    Removed(u32),
    Retain(u32),
}
```

The delta is a Quill-style sequence of Retain / Added / Removed runs over the array. Built lazily on first observer call by `event_change_set` (`types/mod.rs:937+`) which walks `branch.start` cross-referencing `txn.before_state` and `txn.delete_set`. We will not implement observers in the first Array commit; `ArrayEvent` is documented for completeness.

## 9. Concrete Go translation choices

### File layout

- `internal/types/array.go`, `internal/types/array_test.go`. Mirror of `internal/types/map.go`.
- Doc-level registry: `Doc.Array(name string) *types.Array` parallel to `Doc.Map`. Same `roots map[string]*block.Branch` table; the Branch's `TypeRef` distinguishes — `block.TypeRefArray` vs `block.TypeRefMap`.

### `Array` struct

```go
// internal/types/array.go
type Array struct {
    branch *block.Branch
}

func newArray(branch *block.Branch) *Array { return &Array{branch: branch} }

func (a *Array) Len(_ doc.ReadTxn) int { return int(a.branch.BlockLen) }
```

Same shape as `Map`: thin pointer-wrapper. **Branch is shared between Map-like and Array-like usage — both use the same `Branch` struct; only the `TypeRef` field tells them apart.** `Array` reads/writes `Branch.Start` (the linked-list head) and `Branch.BlockLen`; it never touches `Branch.Map`. `Map` reads/writes `Branch.Map`; it never touches `Branch.Start`. Same Branch struct, two disjoint usage modes — confirmed by `branch.rs:184-192`.

### Position-resolution — port

Single function, no `BlockIter` cursor surface for v1. We do the eager-split variant (`Branch::index_to_ptr` style) because we have no incremental-iteration use case in the public API yet:

```go
// internal/types/array.go (unexported helper)
//
// findPosition walks branch.Start counting countable, non-deleted items.
// On interior hit, splits the target item via store.SplitBlock and returns
// (left, right) where left is the new left half and right is the new right
// half (item to the right of the insert/start-of-delete point).
//
// On boundary hit (index == sum of widths so far), returns (item, item.Right).
// On empty / index == 0, returns (nil, branch.Start).
// On out-of-range, returns (nil, nil, false).
func findPosition(
    ctx block.IntegrateContext,
    branch *block.Branch,
    index int,
) (left, right *block.Item, ok bool) {
    if index == 0 {
        return nil, branch.Start, true
    }
    remaining := index
    cur := branch.Start
    for cur != nil {
        if !cur.Deleted() && cur.Countable() {
            length := cur.Len  // ContentLen for Text once we have it; same for Array
            if remaining == length {
                return cur, cur.Right, true
            }
            if remaining < length {
                // Mid-block: split at remaining. Existing primitive:
                newRight := ctx.Store().SplitBlock(cur, remaining)
                return cur, newRight, true
            }
            remaining -= length
        }
        cur = cur.Right
    }
    if remaining == 0 {
        // Append case: walked all the way, sum matched exactly. cur is nil here
        // because we fell off the end; the last non-deleted item is left.
        // Caller should special-case append separately for clarity (see below).
        return nil, nil, true
    }
    return nil, nil, false
}
```

**Note**: the append case (`index == Len`) is cleanest as a separate fast path before calling `findPosition`, so `findPosition` only handles `0 < index < Len`. Pseudocode:

```go
func (a *Array) Insert(tx *doc.TransactionMut, index int, value any) error {
    if index < 0 || index > int(a.branch.BlockLen) {
        return fmt.Errorf("ygo: array index %d out of range [0, %d]", index, a.branch.BlockLen)
    }
    var left, right *block.Item
    switch {
    case a.branch.BlockLen == 0:                                // empty
        left, right = nil, nil
    case index == 0:                                            // prepend
        left, right = nil, a.branch.Start
    case index == int(a.branch.BlockLen):                       // append
        left, right = a.lastLiveItem(), nil
    default:                                                    // middle
        var ok bool
        left, right, ok = findPosition(tx, a.branch, index)
        if !ok { return fmt.Errorf("ygo: findPosition failed for index %d", index) }
    }
    content, err := contentFromAny(value)
    if err != nil { return err }
    item := block.NewArrayInsertItem(tx, a.branch, left, right, content)
    return item.Integrate(tx, 0)
}
```

`lastLiveItem()` walks `branch.Start` to find the rightmost non-deleted item; cost O(N) like everything else — when search markers land, this becomes a marker hit.

### `block.NewArrayInsertItem` factory

Mirror of `NewMapInsertItem` but with `parent_sub == ""`, `right` populated from the caller, and `right_origin = right.Id` (vs. nil for Map):

```go
// internal/block/item_factory.go
func NewArrayInsertItem(
    ctx IntegrateContext,
    parent *Branch,
    left, right *Item,
    content Content,
) *Item {
    var origin, rightOrigin *ID
    if left != nil { lid := left.LastID(); origin = &lid }
    if right != nil { rid := right.Id;     rightOrigin = &rid }
    return &Item{
        ID:          ctx.NextID(),
        Origin:      origin,
        Left:        left,
        Right:       right,
        RightOrigin: rightOrigin,
        Parent:      Parent{Branch: parent},
        ParentSub:   "",
        Content:     content,
    }
}
```

`Item.Integrate` (already ported) handles the `parent_sub == ""` branch: insert into linked list between `left` and `right`, increment `parent.BlockLen += item.Len`, increment `parent.ContentLen += item.ContentLen`. We do NOT need to touch `parent.Map`.

### Insert API surface

```go
// single value
func (a *Array) Insert(tx *doc.TransactionMut, idx int, value any) error

// batched — packs all values into one ItemContent::Any squashed run
func (a *Array) InsertRange(tx *doc.TransactionMut, idx int, values []any) error

// shorthands
func (a *Array) Push   (tx *doc.TransactionMut, value any) error  // = Insert(tx, Len, value)
func (a *Array) Unshift(tx *doc.TransactionMut, value any) error  // = Insert(tx, 0, value)
```

`InsertRange` checks each value is in our Any-compatible set (numbers, strings, bool, nil, []byte, plus nested map/array of same). If any value is not, fall back to per-value `Insert` loop — matches the yrs constraint that `RangePrelim` requires `V: Into<Any>` and that nested prelims go through `ArrayPrelim`'s per-item `push_back`.

### Get / Remove / RemoveRange

```go
func (a *Array) Get(_ doc.ReadTxn, idx int) (any, bool) {
    if idx < 0 || idx >= int(a.branch.BlockLen) { return nil, false }
    item, offset, ok := a.itemAt(idx)        // walk + return (item, intra-block offset)
    if !ok { return nil, false }
    return item.Content.GetAt(offset)        // mirrors Content.GetLast but indexed
}

func (a *Array) Remove(tx *doc.TransactionMut, idx int) error {
    return a.RemoveRange(tx, idx, 1)
}

func (a *Array) RemoveRange(tx *doc.TransactionMut, idx, length int) error {
    if idx < 0 || idx+length > int(a.branch.BlockLen) {
        return fmt.Errorf("ygo: range [%d, %d) out of bounds [0, %d)", idx, idx+length, a.branch.BlockLen)
    }
    // Split at start if mid-block; split at end if mid-block; tx.Delete every fully-covered item.
    // Direct port of BlockIter::delete with rel/len handling.
    ...
}
```

`Content.GetAt(offset)` is a new helper alongside `Content.GetLast`; for `ContentAny(vec)` it's `vec[offset]`, for `ContentString(s)` it's the rune at offset, for single-element variants it's just the value. Add as a method on `block.Content`.

### Value extraction — shared with Map

Move `contentFromAny` and `contentToAny` (currently in `internal/types/map.go`, judging by the Map port notes) into a shared `internal/types/value.go`. Both `Map.Set`/`Get` and `Array.Insert`/`Get` use the same Any-subset; duplicating would drift. **Recommend**: refactor in this commit, since we now have two callers.

## 10. What we will NOT port in the first Array commit

| Feature | Why defer | Unblocking condition |
|---|---|---|
| Search markers | yrs main does not implement them; we are at parity by skipping. | After benchmarks demonstrate O(N) walk is a measurable cost. |
| Observers (`ArrayEvent`, `Change` enum, `event_change_set`, deep observers) | Same reason as Map: requires Branch.observers + transaction-end emission. | Same commit as Map observers. |
| `ArrayPrelim` recursive nested-type construction | Requires `Prelim` trait abstraction and the `In`/`Out` enum. We will hand-code `MapPrelim`/`ArrayPrelim`-equivalent constructors as Go functions in the same commit, but without the trait machinery. | When we add the public `In`/`Out` API surface (post-Day 24). |
| `move_to`, `move_range_to` (`array.rs:344-415`) | Requires `Move` content variant + `StickyIndex` + `Assoc` + the `BlockIter` move-stack machinery + GC interactions with `txn.prev_moved`. Net new subsystem. | Whenever we decide to ship Move semantics — historically the last shared-type feature added in any Yjs port. |
| `BlockIter` cursor as a public API | We don't need an incremental iterator; one-shot `Insert`/`Get`/`RemoveRange` cover the surface. `iter()` is implemented as a simple `for i := 0; i < a.Len(); i++ { a.Get(...) }` — O(N²) total but we accept that for v1, since search markers will fix it later. | Same time as search markers, or earlier if user asks for a streaming iterator. |
| `iter` value-unpacking for `KindEmbed` / `KindType` / `KindDoc` / `KindMove` | None of those Content variants resolve to a usable `any` yet in our `contentToAny` helper. | One commit per subsystem. |
| `get_as` (serde-deserialize-into-typed) | Out of scope; not a Yjs parity feature. | Indefinitely. |
| `to_json` bulk-read fast path | Convenience; the iter-then-collect path is correct, just slower. | After observers / encoder mature. |

## 11. Tests for the first Array commit (`internal/types/array_test.go`)

1. **Empty + Insert at 0 + Get** — `a.Insert(tx, 0, "x")`; `a.Len()==1`; `a.Get(tx, 0) == "x", true`. Verify `branch.Start` points at the new Item with `Left == nil`, `Origin == nil`, `Right == nil`, `RightOrigin == nil`, `BlockLen == 1`.
2. **Append (`Push`)** — push 3 values. Verify `branch.Start.Right.Right == third`, `third.Right == nil`, `BlockLen == 3`. Check linked-list integrity: each Item's `Origin == prev.LastID()`, `Left == prev`.
3. **Prepend (`Unshift`)** — start with one item, `Unshift("a")`. Verify `branch.Start == "a"`, `"a".Right == prevHead`, `prevHead.Left == "a"`, `BlockLen == 2`.
4. **Mid-insert with split** — `InsertRange(tx, 0, [1,2,3,4])` produces a single Item with `ContentAny([1,2,3,4]), Len=4`. Then `Insert(tx, 2, "X")`. Verify the run was split into a `[1,2]` Item and a `[3,4]` Item with `"X"` between them; total `BlockLen == 5`; `Get(0..4)` returns `[1, 2, "X", 3, 4]`.
5. **Remove single** — start with `[a,b,c,d]`; `Remove(tx, 1)`. Verify `BlockLen == 3`, `Get(0..2)` returns `[a, c, d]`. Verify the deleted Item is still in the linked list (tombstone) but `Deleted() == true`.
6. **Remove range, fully covered** — `[a,b,c,d,e]`; `RemoveRange(tx, 1, 3)` removes `b,c,d`. `BlockLen == 2`; `Get(0..1)` returns `[a, e]`.
7. **Remove range with mid-block boundaries** — `InsertRange(tx, 0, [1,2,3,4,5])` (single squashed Item), then `RemoveRange(tx, 1, 3)`. Should split into `[1] | [2,3,4] | [5]`, tombstone the middle. `Get` returns `[1, 5]`. Verify three Items in the linked list, middle one tombstoned.
8. **Insert via Push then read back by index** — Push 10 values; for each i, `Get(i)` returns the i-th pushed value.
9. **Length consistency under squashing** — InsertRange of 5 values yields `Len == 5` even though only one Item exists. Confirms `branch.BlockLen` is maintained correctly by `Item.Integrate` on positional inserts.
10. **Two Doc instances, manual Item exchange (no encoder yet)** — replicate the Map cross-doc test pattern: build Items on doc1, push them through doc2's IntegrateContext. Concurrent inserts at the same index from clients 1 and 2 must converge to the same ordering on both sides (lower client-id wins per YATA). Concurrent inserts at distinct indices both visible on both sides.

These exercise the new code paths (`findPosition`, `NewArrayInsertItem`, `RemoveRange` split arithmetic, `BlockLen` maintenance) without depending on encoding, observers, or nested-type recursion.

## 12. Open questions / FIXMEs in the source

- `block_iter.rs:166` — `//TODO: we need to ensure to iterate further if this.currMoveEnd === null`. Move-related, not in our v1 scope.
- `array.rs:332` — `//TODO: we could probably optimize this step by not serializing to intermediate Any value` (about `get_as` going through `to_json` then `from_any`). Affects only the serde path we are not porting.
- `array.rs:1485` — `#[ignore] //TODO: investigate (see: https://github.com/y-crdt/y-crdt/pull/266)` — an ignored test; out of scope.
- **Search markers**: not implemented in yrs; not flagged as a TODO either. Open question for our port — leaving as "defer to benchmark" per the contract.
- **`Branch::insert_at` vs. `BlockIter::insert_contents` duplication**: both exist in yrs and do similar work; the Array trait uses BlockIter, but the older `Branch::insert_at` lives on. For our port we collapse to one `Array.Insert` path (no `BlockIter`).
- **No explicit empty-`branch.Start` fast path in `BlockIter::try_forward(0)`** — relies on the `len == 0 && next_item.is_none() => true` short-circuit at line 106. We replicate this in `findPosition` via the `branch.BlockLen == 0` check before calling.
