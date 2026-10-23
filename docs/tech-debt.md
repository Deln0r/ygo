# Tech debt

> Items intentionally deferred or acknowledged as incomplete during initial implementation. Each entry says what, why, when to address, and where in the source to find the relevant code or upstream reference. Update as items are resolved or new ones are accumulated.

## Block layer

### Item.Splice does not rewrite parent.Branch.Map for map-key tail items (resolved)

- **Resolved by:** `Item.Splice` makes the right half its key's winner when the split item was (yjs `splitItem`), and `Item.TrySquash` points the map at the merged item when it absorbs the winner (yjs `tryToMergeWithLefts`). Both had been left from the time `Branch` was a stub, and both broke convergence: a merged winner left the map on an item no longer in the store, so replicas diverged for good; a split winner made the next local `Set` lose to the key's history (`TestMap_WinnerAfterMerge`, `TestMap_WinnerAfterSplit`, yjs bytes).

### Item.Repair pass not implemented (partially mitigated)

- **Where:** missing helper in `internal/block`.
- **What:** yrs's `Item::repair(store)` (`block.rs:1368-1431`) runs before integrate during update apply. It resolves the new item's `Left` from `Origin` (via `MaterializeCleanEnd`), `Right` from `RightOrigin` (via `MaterializeCleanStart`), and fixes up an Unknown parent by inheriting from a resolved neighbour.
- **Mitigation today:** types-layer constructors inline the Origin → Left resolution they need (`Map.Set` reads `branch.Map[key]` directly to compute Origin). No standalone Repair helper exists.
- **When to address:** with the V1 update decoder layer, where incoming items arrive with raw Origin / RightOrigin IDs and need full pre-integrate resolution including parent-Unknown inheritance.

### Item.Integrate offset > 0 path stubbed

- **Where:** `internal/block/integrate.go` `Item.Integrate` first branch.
- **What:** when `Update.integrate` applies an item whose first `offset` clocks are already known locally (the suffix is the actual delta), yrs trims the item's left side via `get_item_clean_end` + `materialize` + `Content.splice` (`block.rs:569-583`). We return false without doing this.
- **Impact today:** none — no caller uses offset > 0 yet.
- **When to address:** with the V1 update decoder, which is the only producer of partial-offset integrates.

### Item.Integrate Named/ID parent resolution (resolved)

- **Was:** updates carrying ParentNamed or ParentID silently dropped.
- **Resolved by:** Repair handles all three parent kinds. ParentNamed via `ctx.GetOrCreateBranch`. ParentID via lookup of the parent Item, extraction of its embedded Branch from KindType Content (see `internal/block/repair.go`). Items whose ParentID points at an unseen clock queue in `encoding.Pending` and integrate on the next Apply that satisfies the dependency (proven by `TestNested_OutOfOrderApply_DrainsViaPending`).

### Item.Integrate Move/Subdoc/Weak/Format integrations not handled

- **Where:** `internal/block/integrate.go` `Item.Integrate` content-switch.
- **What:** yrs's integrate handles five content-specific paths (`block.rs:786-818`):
  - `KindMove`: re-integrates left+right moved ranges if they share a Move pointer.
  - `KindDoc`: registers child subdoc, emits `subdocs.added` / `loaded`.
  - `KindFormat`: sets `Branch.HasFormatting` (yjs `ContentFormat.integrate` sets `_hasFormatting` and drops the search markers; ygo keeps its markers for positions and walks from the start for text inserts).
  - `KindType`: weak-link source materialization (only with `weak` feature).
  - Weak-link inheritance from left when overriding map-key.
  We handle only the `KindDeleted` case (set FlagDeleted) and skip the rest.
- **When to address:** Move post-MVP; Subdoc post-MVP; Weak post-MVP.

### TransactionMut.changedTypes drops the parent_sub dimension

- **Where:** `internal/doc/transaction.go` `TransactionMut.AddChangedType` and `changedTypes` field.
- **What:** yrs records `(parent, parent_sub)` pairs so observers can fire per-map-key. Our map drops `parent_sub` and only keys by `*Branch`, so observer dispatch will conflate map-key changes that happen in the same transaction.
- **Impact today:** none — no observers exist.
- **When to address:** with the observer subsystem. Restructure to `map[*Branch]map[string]struct{}` (nil sub-key for positional changes) or a `[]changeRecord` slice.

### TransactionMut.deletedIDs is too narrow for the wire delete set (resolved)

- **Was:** the transaction recorded deleted item IDs only, losing their lengths, so no wire delete set could be built from it.
- **Resolved by:** `TransactionMut.DeletedRanges` records `(ID, Len)` at each `Delete`, before squash can merge the tombstone with a neighbour; content that arrives already deleted (`ContentDeleted`) goes through `Delete` too, as yjs `ContentDeleted.integrate` adds it to the delete set. `EncodeTransactionUpdate` builds the per-transaction delete set from it, and the UndoManager captures from it.

## Types layer (Map and beyond)

### Observer subsystem: XML-type events not yet wired

- **Where:** `internal/types/event.go`.
- **What:** `Map.Observe`, `Array.Observe`, `Text.Observe`, and `Map`/`Array.ObserveDeep` are shipped with full YMapEvent / YArrayEvent / YTextEvent parity (cross-checked against captured yjs@13.6.31 output: map add/update/delete + oldValue; array insert/delete/retain with trailing-retain trim; text formatting-aware delta with attribute runs; observeDeep path bubbling with map-key and array-index segments). Events fire on local and remote transactions. gomobile exposes `Text.ObserveChanges` / `Map.ObserveChanges` delivering Quill-style JSON. Still missing: dedicated XmlElement / XmlText events (they are positional branches and currently route to the Array/Text dispatch by TypeRef, which is approximately right but has no XML-specific event shape or test), and `Text.ObserveDeep` (only Map/Array expose ObserveDeep; text rarely nests types).
- **When to address:** when an XML-tree adopter needs change events, add an XmlEvent shape; add `Text.ObserveDeep` if a nesting use case appears.

### Map.Get value extraction handles only Any/String/Binary

- **Where:** `internal/types/map.go` `extractValue`.
- **What:** the user-visible value for `Map.Get(key)` is unpacked from `Content`. We handle `KindAny` (returning `Anys[0]`), `KindString` (returning `Str`), and `KindBinary` (returning `Bytes`). yrs additionally supports `KindEmbed`, `KindType` (nested shared types), `KindDoc` (subdocs), and the `Out`/`Value` enum dispatch.
- **When to address:** with Array, Text, and the typed `Any` replacement (covered separately under "Any type is a placeholder"). Map gets its full value coverage as a free side-effect.

### Map.Len is O(N) over branch.Map

- **Where:** `internal/types/map.go` `Map.Len`.
- **What:** `Len` iterates the entire `branch.Map` skipping tombstoned entries. yrs has a TODO at `map.rs:158` about caching live-count on the Branch; we'd inherit that optimization for free if/when Branch grows the cache.
- **When to address:** if benchmarks show Len in hot paths.

### Array / Text position resolution search-marker cache (resolved)

- **Was:** every Insert / Delete walked `branch.Start` linearly to translate a user-facing index into a `*Item`. O(N) per op; B4 real-world LaTeX trace (259,778 edits) took ~84 s vs yrs's sub-10 s.
- **Resolved by:** `internal/block/search_marker.go` adds a bounded-LRU per-branch marker cache (cap 80, matching yrs's INTERNALS.md sweet spot). Each marker stores `(item, user-facing-index, timestamp)`. `Array.findInsertPosition` and `Text.findTextPosition` consult `branch.Markers.Nearest(idx)` and walk forward from the closest marker instead of from `branch.Start`. `Insert` / `Delete` call `ShiftAfter` / `ShrinkAfter` to keep marker indices accurate as positions move. `Item.Integrate` invalidates positional markers on its parent to handle the remote-apply path (which doesn't go through the local API and would otherwise leave markers stale).
- **Impact:** B4 dropped 84 s → 10.5 s (**8× speedup**); within ~1.1× of yrs published numbers. Random-position B1.4 / B1.5 / B1.11 each gained ~30-35%. Prepend-only workloads (B1.3, B1.10) regressed ~10-60% in absolute time (sub-1 ms scale) — markers cost a tiny bookkeeping overhead they can't recoup because idx=0 short-circuits the marker path. Acceptable trade.

### Array.Range value extraction handles only Any/String/Binary

- **Where:** `internal/types/array.go` `extractValueAt`.
- **What:** same shape as Map's `extractValue`. KindEmbed / KindType (nested types) / KindDoc (subdocs) / KindMove return nil on Get/Range.
- **When to address:** with the respective subsystems (nested-type construction, subdocs, Y.Array.move).

### Array.Move not implemented (Y.Array.move equivalent)

- **Where:** missing on `internal/types/array.go`.
- **What:** Y.Array supports `move(srcIdx, dstIdx)` to relocate an element. yrs implements via the ContentMove variant; we do not encode/decode/integrate Move at all.
- **When to address:** post-MVP per ROADMAP. Pre-conditions: ContentMove encode/decode in encoding layer, Move integration in Item.Integrate (deferred per integrate.md).

### Text rich-text formatting (resolved)

- **Was:** Text supported only plain-text Insert/Delete/String/Length. No format markers, no embeds, no Quill-style delta API.
- **Resolved by:** `internal/types/text_format.go` ships `Text.InsertWithAttributes(idx, str, attrs)`, `Text.Format(idx, len, attrs)`, `Text.InsertEmbed(idx, value)`, `Text.Range(fn)`, `Text.ToDelta()`, `Text.ApplyDelta(ops)`. KindFormat and KindEmbed wire format wired into `internal/encoding/content_codec.go` (note: both use `writeJSON = varstring(JSON.stringify(v))` per yjs UpdateEncoder.js, NOT lib0 Any). Tests cover open/close markers, format on existing range, attribute-clearing, embeds in delta, cross-client convergence, ApplyDelta dispatch over insert/retain/delete/embed/attrs, ToDelta→ApplyDelta round-trip.

### Text.Range / Text iteration not implemented

- **Where:** missing on `internal/types/text.go`.
- **What:** Map and Array expose `Range(fn)`; Text only exposes `String()` for full read. A streaming `Text.Range(fn func(chunk string, attrs *Attrs) bool)` would let observers / encoders iterate per-Item segments without materializing the full string.
- **When to address:** with rich-text formatting (rich-text Range yields chunks plus current attrs).

### Text Insert / Delete walk text content twice on mid-block hit

- **Where:** `internal/types/text.go` `findTextPosition`.
- **What:** when index lands inside a String item we call `Content.Len(KindString)` (which calls `utf16.Length` → walks the string) AND, immediately after the split point check, `Store.SplitBlock` → `Item.Splice` → `Content.splitString` → `utf16.SplitAt` (which calls `utf16.ByteOffset` → walks again). Two O(N) walks per split.
- **Why deferred:** premature; falls out of the broader storage-layout decision above.
- **When to address:** with the storage layout benchmark.

### Shared types embedded in a text are invisible to Range and ToDelta

- **Where:** `Text.Range` / `ToDelta` (`internal/types/text_format.go`).
- **What:** a shared type inserted into a text (yjs allows a Y.Map as an embed) counts in `Length` but `Range` skips `KindType` items, so `ToDelta` leaves it out: yjs text "ab" with a map embedded at index 1 has length 3 in both, and ygo's delta is "ab" where yjs gives "a", the map, "b" (measured with a yjs 13.6.33 update).
- **When to address:** with the rich-text insert work.

### XmlElement.ToString lower-cases tags without context

- **Where:** `XmlElement.ToString`.
- **What:** it uses `strings.ToLower`; yjs uses `toLocaleLowerCase`, which applies contextual rules such as the Greek final sigma (`ΟΣ` gives `ος` in yjs, `οσ` here). ASCII tag names, which is what HTML and editor schemas use, come out the same.
- **When to address:** if a document uses non-ASCII tag names.

## Encoding layer (V1 wire format)

### StateVector / IdSet have no JS Yjs cross-language byte-equality fixtures

- **Where:** `internal/encoding/state_vector_test.go`, `idset_test.go` — only Go round-trip tests today.
- **What:** lib0 fixtures pass byte-equality with JS lib0 directly. SV and IdSet encode call lib0 primitives, so the per-byte arithmetic is correct, but we have not run a JS-encodes / Go-decodes round-trip against actual JS Yjs output.
- **Why this is a real gap, not a paranoia:** per `update-v1.md` gotcha 1, sort direction asymmetry is the easiest place to silently produce bytes JS Yjs rejects. Our determinism choice (sort ascending) matches yrs's BTreeMap iteration for IdSet but differs from JS Yjs's HashMap insertion order for StateVector. Decoding either way works (varuint-pair list is order-independent on read); encoding direction matters only for byte-equality, which is what fixtures would catch.
- **When to address:** with the Update encode/decode commit. Once Update bytes round-trip against JS Yjs, SV and IdSet are exercised end-to-end through real wire updates and the gap closes naturally.

### Update encode / decode partial — no slicing at SV boundary (resolved 2026-10-01)

- **Was:** `EncodeDiff` / `EncodeDiffV2` dropped cells the remote fully knew but wrote the boundary cell whole. Marked low priority in May; commit-time squash (June) then merged a writer's appends into one growing cell, so every "incremental" diff carried the writer's whole history: 27 bytes growing to 1720 over 200 appends, measured by a third-party evaluation that chose the other Go port for it.
- **Resolved by:** `writeClientsStructs` (`internal/encoding/update.go`, V2 twin in `update_v2.go`), the yjs `writeStructs` algorithm: per client the header clock is `max(remoteClock, first cell start)`, found with `FindPivot`, and the first cell is written from that offset (`encodeCellFrom` / `itemFrom`: a copy whose origin is the clock before the cut and whose content drops the prefix; a GC run shortened by the offset). `Doc.OnUpdate` / `OnUpdateV2` reuse it from the transaction's start state. Byte-identical to `Y.encodeStateAsUpdate(doc, sv)` / V2 for every state vector on a grid over each scenario's clocks, including inside a surrogate pair and inside a collected run (`testdata/update-event-fixtures.json`).

### Remaining update-event divergences (pinned)

- **Where:** `knownUpdateEventDivergences` in `update_event_fixture_test.go`.
- **What:** `undo-keys-from-dependent-blocks` and `cleanup-deletions-from-dependent-blocks`: an update's blocks integrate in passes (entry "An update's blocks integrate in passes, not by following dependencies" below). `insert-into-root-typed-after-formatting`: a root typed after formatted text arrived inserts as a formatted text (entry of that name below).
- **Resolved since:** deleting a nested type now deletes the items inside it (`deleteTypeContent`, yjs `ContentType.delete`) and undo restores it (October 2026, three pins removed). With it came the rest of yjs's model for nested types: the UndoManager captures only when a type in scope that still exists changed (yjs `changedParentTypes`), keeps the ancestors of what it keeps and releases them when steps are dropped, before it keeps a new step's deletions and, for `Clear`, before the next transaction body, handler or GC can keep or collect an item (`keepItem`, `clearUndoManagerStackItem`, `doc.QueueUnderLock`), restores into a parent's live copy and rechecks scope there (`redoItem`, `popStackItem`), restores before it deletes and in the order clients first deleted, counting the whole transaction as yjs does, with remote updates applied blocks first and clients from the highest ID down as yjs reads them, deletes a map's keys in the order they were first set (`Branch.MapOrder`, yjs `_map`), ends the capture group on undo (`stopCapturing`), and a collected type drops its references to its old items (`ContentType.gc`). Later in October the kept-tombstone pin went too: kept items merge as yjs `Item.mergeWith` merges them (carrying `keep`), and undo cuts the items it restores at the step's range ends (yjs `iterateStructs`), so a merged tombstone is never restored beyond what a step deleted. The non-merge had not only cost structs: text typed right after a restored copy stayed apart from it, and undoing the original insertion left that text where yjs deletes it (scenarios `undo-insertion-merged-into-restored-copy`, `undo-array-restored-copy-merge`).
- **Resolved since (1.29.0):** the format cleanup after a remote transaction (`cleanupYTextAfterTransaction`, `internal/types/text_cleanup.go`) runs as yjs runs it: a local transaction with origin nil, opened after the remote transaction's after-transaction handlers and before its GC, whose update follows the remote one; the `concurrent-formatting` pin is gone and 16 scenarios cover the cleanup (three peers, different keys, an inner gap it does not reach, the contextless path, a deleted marker calling for the full cleanup, both client orders of a mixed delete, deletions walked in the order the transaction made them, two texts, an untyped root that runs no cleanup and one the cleanup reaches, a root typed late, an XmlText, undo with and without grouping, redo cleared).
- **When to address:** see its entry.

### Text deltas do not clean up formatting as yjs's delta getter does

- **Where:** `buildTextDelta` (`internal/types/event.go`).
- **What:** yjs computes `YTextEvent.delta` lazily, in a transaction of its own, and deletes format markers it finds redundant while it does (YText.js 662-810). A yjs document whose app reads text deltas in an observer therefore deletes more markers, in extra local transactions, than one that does not; the automatic cleanup after a remote transaction then often finds nothing left. ygo builds deltas eagerly and without side effects. Its documents and events match a yjs document in which no observer reads a delta. Where a yjs observer reads one, the document and the events can both differ. For two peers bolding the same text, the remote event's delta is the same bold retain in both, but yjs's getter then deletes the receiver's own opening marker in a transaction of its own, whose event has an empty delta, and the automatic cleanup finds nothing left; ygo's cleanup deletes the sender's marker, and its event reports the bold retain. Markers that only the getter would delete stay in ygo. Documents converge either way, since every peer applies the deletions it receives.
- **When to address:** if an adopter needs byte-identical state with a yjs editor binding that reads deltas. It needs a lazy delta (an API change) and transactions opened from observers, which ygo does not allow.

### A root typed after formatted text arrived inserts as a formatted text

- **Where:** `Branch.HasFormatting` (`internal/block/stubs.go`), `NewText` / `NewXmlText`, `Text.Insert` and `insertContent` (`internal/types/text.go`, `text_format.go`).
- **What:** a format marker that integrates into a type turns its search markers off (yjs `ContentFormat.integrate` sets `_searchMarker = null`); after that, inserts walk from the start and collect the attributes in effect. A root that receives formatted text while nothing has read it as text holds the markers in a placeholder; `Doc.get` then builds a fresh YText with search markers on. Until the next format marker integrates into it, a yjs insert without attributes starts at a search marker with no attributes in effect and collects only those of the markers it passes from there. Where that marker sits depends on the clocks of the items before the position (`findMarker` steps left over items of one client with consecutive clocks), so text appended after bold text can come out either way in yjs: plain when the text was typed plain and formatted afterwards (the insert steps over the closing `bold: null`), bold when it was typed bold in one call. ygo keeps `HasFormatting` from the placeholder, walks from the start and collects the attributes in effect, so the appended text is bold in both cases. Both are valid operations and peers converge on what was sent; the content differs from what a yjs peer would have written. Pinned scenario `insert-into-root-typed-after-formatting`; a document that types its roots (`ygo.NewText`, `ygo.NewXmlText`) before it applies updates never meets it.
- **When to address:** if an adopter reads roots late and needs yjs's attributes on the next insert. Clearing `HasFormatting` when a root is typed is not enough: the marker path (`findTextPosition`, the no-format branch of `Insert`) assumes a text without format markers, and yjs's result there depends on which search markers exist, so the port has to reproduce `findMarker` and `findNextPosition` over format markers.

### An update's blocks integrate in passes, not by following dependencies

- **Where:** `Pending.integrateAll` (`internal/encoding/pending.go`).
- **What:** yjs `integrateStructs` takes clients from the highest ID down and, when a block depends on a block of another client in the same update, follows into that client at once and integrates just enough of it. `integrateAll` takes clients in the same order, but skips such a block and picks it up in the next pass. The document the update integrates into is the same; the order of the transaction's deletions and of the map keys it sets first can differ, and with it the order an UndoManager restores a tracked update's deletions in (and so the clocks the restored items get), the order deleting such a map walks its keys in, and what the format cleanup after the transaction deletes, since it walks the deletions in that order. It takes one update in which a block depends on a block of another client in that update; the pinned scenario `undo-keys-from-dependent-blocks` shows it with three writers, where undo restores a middle client's key before the dependent one, and `cleanup-deletions-from-dependent-blocks` with three writers whose structs all arrive deleted: yjs's cleanup drops a duplicate bold marker that ygo's keeps, so a ygo document differs from a yjs one until the deletion reaches it from a yjs peer that made it, and peers that are all ygo keep the marker.
- **When to address:** if a scenario needs it; porting the dependency stack means porting yjs's pending handling with it.

### Undo does not restore subdocument references

- **Where:** `redoItem` (`internal/undo/redo_item.go`), `Content.CopyableForUndo`.
- **What:** undo skips `ContentDoc` (and `ContentMove`) items. yjs `ContentDoc.copy` creates a new document with the same GUID and options, so undoing the deletion of a subdocument, or of a map holding one, brings the reference back; ygo restores the map without it.
- **When to address:** with subdocument lifecycle work: a restored reference has to register as a new subdocument and report through `OnSubdocs`.

### UndoManager ignores IgnoreRemoteMapChanges

- **Where:** `undo.Options.IgnoreRemoteMapChanges` / `ygo.UndoManagerOptions`.
- **What:** the option is accepted and stored but not used. yjs `ignoreRemoteMapChanges` lets undo restore a map key over a later write from another client (redoItem takes the key's current winner as left instead of giving up); ygo gives up as with the option off, so `Undo` returns false and the remote value stays.
- **When to address:** with the next UndoManager options work; the restore path for map keys is `redoMapItem`.

### GC of a deleted type merges its children's runs further than yjs

- **Where:** `TransactionMut.gcDeleted` merge pass (`SquashFrom` from the smallest touched clock).
- **What:** yjs merges only around the transaction's delete set and its `_mergeStructs`, so collecting a type whose children were deleted (and released) earlier can leave several adjacent GC runs; ygo collapses them into one. Peers converge on the same content; the V1/V2 state bytes differ (one GC struct of length 4 where yjs writes 2, 1, 1).
- **When to address:** if byte-exact state encoding after such histories is needed.

### Pending queue corner cases

- **Where:** `Update.Apply`, `Pending` (`internal/encoding/pending.go`).
- **What:** (1) yjs retries its pending structs only when an update reaches a clock its `missing` vector names; ygo retries whenever something is queued, so a queued block whose dependency arrived, but below the recorded missing clock, integrates one update earlier than in yjs. (2) When the queue holds two encodings of the same clocks (a live item, then the same clocks as ContentDeleted / GC from a later diff), yjs's merged pending update keeps the later encoding and ygo the first; the delete set then leaves the same document, but with GC off ygo keeps a deleted type and its child where yjs has ContentDeleted and GC, and the update event's delete set covers one more clock.
- **When to address:** if a sync scenario needs byte-exact pending behaviour.

### Subdocument lifecycle events differ from yjs

- **Where:** `Doc.Load`, `TransactionMut.trackSubdocs`, `Doc.OnSubdocs`.
- **What:** (1) `Load` on a subdocument only sets the flag; yjs `load()` opens a transaction on the parent that reports the subdocument as loaded, so ygo never reports it in `Loaded`. (2) A locally added subdocument is not reported as loaded (a new yjs `Y.Doc` has `shouldLoad` true), and an autoLoad subdocument added and deleted in one transaction reports nothing where yjs reports it loaded. (3) A subdocument deleted and re-added with the same GUID in one update drops out of `Subdocs()` (the added pass registers it, the removed pass for the old reference unregisters the same GUID); `GetDoc` still works.
- **When to address:** with subdocument provider work; (1) needs a parent transaction from `Load`, which must not be called with the parent's lock held.

### Commit-time squash runs before after-transaction handlers (resolved)

- **Resolved by:** `TransactionMut.Commit` now runs in yjs `cleanupTransactions` order: observers, after-transaction handlers, GC (`gcDeleted`), then the merge of the transaction's new blocks (`squashNewBlocks`) and of the pieces of items it split (`mergeSplits`, yjs `_mergeStructs`, from `BlockStore.TakeSplits`). The old order let a handler see merged tombstones and, once kept items could merge, let a deleted item merge into a kept neighbour before GC and keep its content (`TestCommit_CollectsBeforeMergingIntoKept`); the missing re-merge left split pieces apart, so state bytes differed from yjs after a remote insert inside an item (`TestCommit_RemergesSplitPieces`).

### Undo cuts structs only at the ranges in its scope

- **Where:** `applyStackItem`, `setKeep` (`internal/undo/undo.go`).
- **What:** yjs walks the whole delete set and insertion set it captured, ranges outside the manager's scope included, and so cuts merged structs at those ranges too (the restored pieces carry `redone` and stay apart). ygo records and cuts only the ranges in scope, so a tombstone merged across such a range stays one struct: the content is the same, the struct layout and its encoding differ.
- **When to address:** if byte-exact layouts after multi-scope undo are needed; it needs the out-of-scope ranges kept on the step.

### Strings with invalid UTF-8

- **Where:** documents stored by a release before v1.26; `lib0.ReadVarStringAsStored` for stored bytes; values reachable after the call.
- **What:** (1) Since v1.27 the API makes every string valid at the call (`utf16.ValidString`, `anyval.Valid` / `ValidJSON`). Content that a release before v1.26 stored with invalid bytes still loads as stored, unchanged, and JS clients still cannot read it; items derived from it keep its bytes (negation markers for a raw format key in effect, undo copies of raw text, keys and names). There is no repair: replacing the bytes after the items have clocks cannot keep replicas consistent (two invalid fragments of one client can merge into a valid character with the old length, a peer that holds the raw bytes never receives the replaced item again, replaced keys can collide with each other and with real U+FFFD keys and turn into deletions). Find affected documents by running `ValidateUpdate` over their stored updates. A raw key stored that way is reachable through iteration only; lookups use the fixed key. (2) A caller that mutates a value after passing it to the API, or one a read returned, or reaches into `Branch` / `Item`, can still put invalid bytes on the wire; that is outside the API contract (package docs). (3) A caller-set `doc.Options.GUID` must be valid UTF-8: reconstruction passes stored GUIDs through the same constructor, so it is not changed.
- **When to address:** a repair tool only together with a way to make every replica take the repaired items.

### Cyclic or very deep values in Map and Array

- **Where:** `encoding.EncodeAny` (`internal/encoding/any_codec.go`).
- **What:** a `Map.Set` / `Array` value that refers back to itself recurses without bound when the document is encoded, a crash that predates the UTF-8 work; a value nested deeper than `anyval.MaxTreeDepth` (1000) is not checked for invalid UTF-8 below that depth. For JSON contents (embeds, format values) a cyclic value becomes `null`, and a deep acyclic one is marshaled whole, so it is checked at every depth.
- **When to address:** with a cycle check in `EncodeAny`, if a caller hits it.

### Lone surrogates in V2 string columns

- **Where:** `lib0.StringDecoder.Read` (`internal/lib0/rle.go`).
- **What:** a V2 update whose string column cuts a surrogate pair in two (JS strings with lone surrogates, which yjs accepts) is refused with `ErrSplitSurrogate`. A Go string cannot hold a lone surrogate, and U+FFFD in its place would change keys and names that yjs keeps apart. The V1 encoding of the same document applies: JS `TextEncoder` writes each lone surrogate as U+FFFD.
- **When to address:** if a V2 producer with lone surrogates turns up; content strings (not keys or names) could then take U+FFFD per half.

### Wire details yjs reads that ygo does not

- **Where:** `internal/encoding/content_codec.go`, `readJSON`.
- **What:** (1) `ContentJSON` (content kind 2, the legacy JSON content) is not decoded; an update holding it fails. (2) Go's JSON decoder turns an escaped lone surrogate (`"\ud800"`) into U+FFFD, where yjs keeps it, so two such JSON keys can become one. (3) Bytes after a complete update are ignored by both yjs and ygo, so the server relays a payload with a trailing remainder as it came; `ValidateUpdate` is the strict check.
- **When to address:** (1) if a document from an old yjs version turns up; (2) and (3) are documented differences.

### Rich-text inserts: differences from yjs that stay

- **Where:** `Text.InsertWithAttributes`, `InsertEmbed`, `ApplyDelta`, `attrValuesEqual` (`internal/types/text.go`, `text_format.go`).
- **What:** (1) `ApplyDelta` treats `{retain: 0}` and `{insert: ""}` as no-ops: a `DeltaOp` cannot say which field was given. yjs runs `formatText` for a zero retain, which moves the cursor across markers that clear a value and, with attributes, can write two markers. Quill does not emit zero retains. (2) A Go map has no key order, so the caller's attribute keys are taken in ascending order (after integer-like keys, which JavaScript enumerates first either way); yjs takes the object's insertion order, which decides the order of the opening markers. (3) Two empty slices cannot be told apart in Go, so one empty slice reused as the value of two markers compares as two objects where yjs's `===` sees one; the gap cleanup after a delete can then keep a marker yjs removes. (4) Attribute keys that name `Object.prototype` members (`toString`, `constructor`, `__proto__`, ...) are not cleared by a yjs insert that omits them, because the inherited property is not `undefined`; ygo clears them like any other key. (5) `Insert`, `Delete` and `Format` return an error for an index or range past the end; yjs appends, deletes what is there, or pads with newlines. `ApplyDelta` follows yjs. (6) ygo has no call that embeds a shared type in a text (yjs `insertEmbed` with a `Y.AbstractType`). (7) yjs `insertText` writes the nulls of cleared attributes into the attributes object the caller passed, so a caller that reuses one object across calls or delta ops sees them in the next call; ygo never writes into the caller's `Attrs`. (8) A retain past the end of the text pads at most `math.MaxInt32` newlines; a longer one fails.
- **When to address:** (1) if `DeltaOp` gains an op kind; the others are API choices.

### Rich-text local inserts at a format boundary (resolved)

- **Was:** plain text typed right before a formatted run went after the run's format marker and took its format, where yjs puts it before the marker; text typed at the end of a run went after the closing marker; an insert with explicit attributes inside formatted text kept the surrounding attributes, where yjs closes them around it; embeds inherited formatting; `ApplyDelta` resolved every op by index.
- **Resolved by:** `insertText` in `internal/types/text_format.go`, a port of yjs `insertText` (null-filled inherited keys in JavaScript property order, `minimizeAttributeChanges`, `insertAttributes`, `insertNegatedAttributes`), used by `Insert`, `InsertWithAttributes`, `InsertEmbedWithAttributes` and `ApplyDelta`, which carries one cursor through the delta with ports of `formatText` (newline padding) and `deleteText` (gap cleanup that corrects the cursor's attributes). `Branch.HasFormatting` (yjs `_hasFormatting`) selects the walk from the start for a text that has had a format marker. `ToDelta` ends an op at every format marker, as yjs `toDelta`. The 8 pinned rich-text fixtures match; 20 new rich-text and 11 update-event scenarios, plus two Go tests with bytes from yjs (an inherited NaN attribute, a Go `int` against a peer's number).

### Delete-set order inside a client, and subdocument order after a queue retry

- **Where:** `Pending.applyDeletes`, `IdSet`, `trackSubdocs`.
- **What:** (1) an update's delete set applies client by client in the order written, but each client's ranges in clock order and a client written twice merged into its first position; yjs applies ranges exactly as written, which can change the order observers fire in. (2) `OnSubdocs` lists added subdocuments by client, highest first; when blocks waiting in the queue integrate in the same transaction after the update's own, yjs lists them in that integration order.
- **When to address:** if a consumer depends on those orders.

### State encodings ignore the pending buffer

- **Where:** `EncodeDiff` / `EncodeDiffV2` (and so `EncodeStateAsUpdate`); `EncodeStateAsUpdateWithPending` exists separately.
- **What:** yjs `encodeStateAsUpdateV2` appends `pendingDs` and the pending structs diffed against the target state vector, so a replica relays what it holds but cannot integrate yet. ygo's state encodings leave the pending buffer out; per-transaction updates correctly exclude it in both.
- **When to address:** with the sync-provider work.

### Item.Repair ParentID (resolved)

- **Was:** Repair returned `ErrParentIDUnresolved` for any ParentID reference.
- **Resolved by:** ParentID arm in `internal/block/repair.go` looks up the parent Item via `ctx.GetItem`, asserts the Content is KindType, and binds `it.Parent = {Kind: ParentBranch, Branch: parent.Content.Branch}`. Missing parents queue in `encoding.Pending` and retry on subsequent Drain passes. Tests in `internal/types/nested_test.go` cover Map-in-Map, Array-in-Map, Text-in-Map, Map-in-Array, deeper hierarchies, wire round-trip, cross-client convergence, and out-of-order delivery.

### Pending update buffer (resolved)

- **Was:** `Update.Apply` returned `ErrMissingDependency` whenever an item's Origin / RightOrigin / Parent-by-ID pointed at a clock the local store had not seen, aborting the whole apply.
- **Resolved by:** `internal/encoding/pending.go` adds `Pending` (per-doc queue of blocks + delete-set entries keyed by client, sorted by clock). `Update.Apply` now folds missing-dep items into Pending and runs `Pending.Drain` in a loop until fixed point. Pending state lives on `doc.Doc` via the opaque `pendingState any` slot (accessed through `TransactionMut.PendingState` / `SetPendingState` so the doc package does not import encoding). Top-level helper `encoding.ApplyUpdate(d, raw)` opens a write txn, decodes, applies, and commits. Inspection helpers: `encoding.HasPending`, `encoding.GetPending`, `encoding.MissingSV` (returns the SV a peer should fetch to drain the queue). 7 tests cover out-of-order delivery, stuck-pending MissingSV, DS-before-item retry, idempotency.
- **Remaining gap:** none for the MVP path. Optimization opportunities exist (batch the Drain inner loop with dependency-bucketed retry instead of per-client linear scan; emit a "this update added nothing" signal so callers can skip the retry pass when the pending buffer is already empty) but they are pure-perf and unblocked by benchmarks.

### Any TLV missing arrays / objects / buffers / bigint / float32

- **Where:** `internal/encoding/any_codec.go`.
- **What:** `EncodeAny` and `DecodeAny` cover null/undefined, bool, string, int (≤ 31 bits), int64-or-larger-as-float64, float64. Tags 116 (binary), 117 (array), 118 (object), 122 (bigint), 124 (float32) are unsupported — DecodeAny returns `ErrUnsupportedAnyTag`; EncodeAny panics for the corresponding Go types.
- **Impact today:** Map.Set with primitive values round-trips. Map.Set with `[]any` arrays, `map[string]any` objects, or `[]byte` (use ContentBinary for binary instead) does not.
- **When to address:** with the Array shared type (which forces full Any coverage end-to-end), or when a real adopter hits the limitation.

### DeleteSet split-on-boundary (resolved)

- **Was:** delete-set apply called `txn.Delete(it)` on the whole item when a wire range covered only a SUBSET of its clocks. Receivers of a partial-delete from a peer who had previously split an item lost MORE than the sender intended; cross-client convergence broke for any pattern where one side has a single contiguous item and the other ApplyDelta's a sub-range delete.
- **Resolved by:** new `applyDeleteRange` helper in `internal/encoding/pending.go` calls `txn.MaterializeCleanStart` (splits at range start) and `txn.MaterializeCleanEnd` (splits at range end-1) before `txn.Delete`. The Drain loop routes every delete-set range through it. 4 dedicated regression tests in `internal/encoding/delete_split_test.go` cover split-at-start (preserve prefix), split-at-end (preserve suffix), split-at-both-ends (preserve prefix + suffix around middle delete), and whole-item degenerate case. `TestApplyDelta_CrossClient_Converges` re-enabled to the full insert+format+partial-delete+insert scenario and passes.

### Update.Apply does not handle Skip blocks — PARTLY RESOLVED (2026-09-05, v1.19.0)

- **Where:** `internal/encoding/update.go` `Update.Apply`.
- **What:** Skip wire records (BLOCK_SKIP_REF_NUMBER = 10) reserve clock space without semantics. We decode them but Apply drops them, so re-encoding from the resulting store does not reproduce them.
- **Both triggers have since fired.** V2 encoding landed, and JS Yjs was measured emitting Skip in V1: `yjs@13.6.32` `mergeUpdates` writes a Skip run for every hole in a client's sequence, for the ordinary case of merging a set that spans a gap. It is not rare, it is the normal shape of a merged update.
- **What is resolved:** the merge path now EMITS Skip runs. `EncodeStateAsUpdateWithPending` (`internal/encoding/merge.go`) encodes the store together with the pending buffer and writes a Skip for each hole between them, in both wire formats, so `MergeUpdates` / `MergeUpdatesV2` reproduce what yjs produces and no longer drop the blocks behind a gap. Cross-language fixtures hand our output to yjs on every push.
- **Remaining gap:** `Apply` still drops an incoming Skip rather than recording the reserved range, so a Skip that arrives and is never followed by the real blocks leaves no trace in the store. That costs nothing today - the blocks behind the hole queue in the pending buffer and carry their own clocks - but it is why re-encoding a document is still not byte-identical to the update that built it when Skips were involved.
- **When to address:** if a scenario turns up where the reserved range itself has to survive a store round trip.

### Content encoding for Embed / Format / Type / Doc / Move / JSON not implemented

- **Where:** `internal/encoding/content_codec.go`.
- **What:** EncodeContent panics on these kinds; DecodeContent returns "unsupported content kind". Map.Get's `extractValue` already handles the read side (returns nil for unknown kinds), so wire-decoded items of these kinds would integrate but be invisible to Map readers.
- **When to address:** Embed with JS Y.Map embedding objects; Format with Y.Text rich-text; Type with nested shared types (Array/Map/Text inside another); Doc with subdocs; Move with Y.Array.move(); JSON is legacy (yrs supports decode, encode is rare).

### Cross-language JS Yjs → Go direction (resolved)

- **Was:** missing `testdata/gen/gen-yjs-update.mjs` + Go fixture test.
- **Resolved by:** Phase B3 fixture wiring. `testdata/yjs-updates.json` captures 8 scenarios (empty doc, single set, multi-key set, all primitive value types, LWW chain, set+delete, set→delete→set, unicode keys/values). `internal/encoding/fixture_test.go::TestFixtures_DecodeApplyJSYjsUpdates` decodes each via DecodeUpdate, applies to a fresh Doc via Update.Apply, and verifies the resulting Map state matches the expected JSON. All 8 pass under `-race`. CI workflow's `fixtures` job regenerates and runs both lib0 and yjs-update tests on every push.
- **What this proves:** bytes that JS Yjs (yjs@13.6.31) produces via `Y.encodeStateAsUpdate(doc)` are byte-equivalent to what our DecodeUpdate accepts as input. The half of binary-protocol-compat that matters most for adoption — being able to receive updates from existing JS Yjs deployments — is verified end-to-end.

### Cross-language Go → JS Yjs direction (resolved)

- **Was:** only JS → Go direction proven. Bytes Go encoded via `EncodeStateAsUpdate` / `EncodeStateAsUpdateV2` had no JS-side validation; subtle wire-format divergence could pass Go round-trip yet fail on JS adopters.
- **Resolved by:** file-based fixture pipeline.
  - `cmd/gen-go-fixtures/` walks 21 scenarios (Map / Array / Text / XmlFragment, mixed primitive types, unicode, multi-byte, deletes) with pinned ClientIDs, captures bytes via both V1 and V2 encoders, writes `testdata/go-updates.json` + `testdata/go-update-v2-fixtures.json`.
  - `testdata/gen/validate-go-fixtures.mjs` reads both fixtures, applies via `Y.applyUpdate` / `Y.applyUpdateV2` against fresh `Y.Doc`s, verifies live state matches the expected snapshot recorded by Go. All 42 scenarios (21 V1 + 21 V2) pass.
  - CI fixtures job regenerates Go fixtures + runs validator on every push; git-diff drift check catches byte-level regressions.
- **What this proves:** binary-protocol compat in BOTH directions for V1 AND V2. JS clients can apply Go-encoded snapshots; Go clients can apply JS-encoded snapshots. Hocuspocus-compat server scenarios where JS clients download Go state are now wire-validated.

### Surrogate-pair split (resolved)

- **Was:** Content.splitString sliced by byte offset; mid-surrogate splits would emit invalid UTF-8.
- **Resolved by:** new `internal/utf16` package with `Length`, `ByteOffset`, `SplitAt`. `Content.Len(KindString)` now returns UTF-16 code unit count via `utf16.Length` (matches yrs `Item::new` and JS Yjs Item.Len semantics, gotcha 1 from types-text.md). `Content.splitString` calls `utf16.SplitAt` which performs the JS Yjs U+FFFD replacement on mid-surrogate splits (yrs has the same code commented out at `block.rs:1940-1948`; we follow JS Yjs not yrs per types-text.md gotcha 3). Test `TestText_InsertSurrogateSplit_UsesU_FFFD` proves the behaviour.

### Wire info bit 4 reserved, no decoder check

- **Where:** `internal/block/flags.go` (constants only) and the future update decoder.
- **What:** bit 4 (`0b0001_0000`) of the Item info byte is currently unused — content kinds max at 11 (`0b1011`), all presence flags live in bits 5-7. If a future Yjs version starts using bit 4, our decoder must NOT silently mask it.
- **Why deferred:** the V1 decoder doesn't exist yet.
- **When to address:** when implementing the V1 decoder.
- **Action item at that time:** assert `info & 0b0001_0000 == 0` and return a versioning error if it's set; carry the failure into a fixture-based regression test.

### ContentString storage layout: O(N) UTF-16 walk per slice

- **Where:** `internal/block/content.go` `Content.Str string`, `internal/utf16/utf16.go` `ByteOffset` / `SplitAt`.
- **What:** strings stored as Go `string` (UTF-8). UTF-16 offset → byte offset translation walks chars on every call. Same O(N) cost yrs accepts (`SplittableString::block_offset` walks `chars()` per call). Two faster layouts when benchmarks demand: `[]uint16` storage (zero-cost slicing, memory cost) or precomputed `[]uint16Index→byteIndex` index for blocks past N units.
- **Why deferred:** matches yrs's choice; cost is invisible until B3 (text-heavy) benchmarks land.
- **When to address:** after `dmonad/crdt-benchmarks` B3 port shows text editing in hot path. Pair with the "Text Insert / Delete walks twice" entry below.

### Any type is a placeholder (partially resolved)

- **Where:** `internal/block/stubs.go` `type Any = any`.
- **Was:** opaque `any` with encoder/decoder supporting only nil/bool/string/int*/float64.
- **Partially resolved by:** `internal/encoding/any_codec.go` now covers all common lib0 Any variants — `[]byte` (binary, tag 116), `[]any` (array, tag 117), `map[string]any` (object, tag 118 with deterministic alphabetical key order so wire bytes stay reproducible across runs), `float32` (tag 124), and BigInt decode (tag 122 → `int64` via 8-byte BE, matching `writeBigInt64`). Per-variant round-trip tests in `any_codec_test.go`. B3.2 benchmark un-blocked and running.
- **Remaining:** native BigInt encoding from Go (would need an explicit `BigInt` wrapper type so we can distinguish from `int64`; defer until an adopter actually needs to send BigInts from Go). `Any` is still a `type Any = any` alias rather than a tagged union — replacing with a proper union would be a public API change worth its own commit when the surrounding ergonomics make sense.

### Forward-dependency stubs are empty types

- **Where:** `internal/block/stubs.go` (`Branch`, `Move`).
- **What:** placeholder `type X struct{}` definitions so the block package compiles. (`Doc` was previously here; now lives in `internal/doc`. The block-layer `Doc` stub is now only referenced by `block.Content.Doc` / `block.Content.ParentDoc` for `KindDoc` payloads, which is read-only data the block layer never inspects.)
- **Why deferred:** by design — the block layer references these only through pointers and never inspects their internals. Real definitions land with their owning layers.
- **When to address:** `Branch` with the types layer; `Move` post-MVP per ROADMAP. The `block.Doc` stub stays even after `internal/doc.Doc` lands, because the block layer can't depend on the doc layer (would cycle); we'll bridge them through an interface when sub-document support arrives.

## Public API surface

### gomobile bind subset (iOS resolved; Android NDK pending)

- **Was:** the marquee "pure-Go = gomobile bind works" claim was structurally correct (no CGO) but operationally untested; an adopter running `gomobile bind github.com/Deln0r/ygo` would find most of the rich API silently filtered out (`any` / `map` / callbacks / non-byte slices).
- **Resolved by (Go-side):** `gomobile/` package exposes the bindable subset only — `Doc` + `Awareness` wrappers with bytes-in/bytes-out methods (NewDoc / NewDocWithClientID / ApplyUpdate / EncodeStateAsUpdate / EncodeStateVector / EncodeDiff / HasPending / MissingSV / NewAwareness / SetLocalState / LocalState / RemoveLocalState / EncodeAll / Apply). 6 tests prove the bytes-only round-trip. Package builds clean with `CGO_ENABLED=0`.
- **Resolved by (iOS toolchain — May 2026):** `gomobile bind -target=ios,iossimulator` produces a valid `Ygo.xcframework` end-to-end on Xcode 16 + Go 1.26 / macOS 26 Apple Silicon. Both slices are real Apple frameworks: `ios-arm64/Ygo.framework` (6.6 MB, real-device) + `ios-arm64_x86_64-simulator/Ygo.framework` (13 MB universal). Auto-generated Objective-C headers (`Ygo.h`, `Gomobile.objc.h`, `Universe.objc.h`, `ref.h`) preserve all the Go doc-comments on every method. Adopters drop the xcframework into Xcode, import via the generated Swift bridge.
- **Resolved by (Android toolchain — May 2026):** `gomobile bind -target=android -androidapi 21` produces a valid 8.4 MB `.aar` containing native JNI libraries for all four standard Android architectures (`arm64-v8a` 3.8 MB / `armeabi-v7a` 3.7 MB / `x86_64` 4.1 MB / `x86` 3.7 MB) plus a `classes.jar` exposing `gomobile.Doc`, `gomobile.Awareness`, and `gomobile.Gomobile` package-facade Java classes. Drop into Android Studio's `app/libs/`, add `implementation files('libs/ygo.aar')` to `build.gradle`, `import gomobile.Doc;`. Verified on NDK 27.0.12077973 + Android SDK platform-21 + Android Studio Ladybug bundled JBR.
- **Both toolchain notes** — exact commands in [gomobile/README.md](/gomobile/README.md). `gomobile bind` needs `golang.org/x/mobile/bind` resolvable in the module graph; main `go.mod` deliberately does not carry the dep, so non-mobile adopters pull nothing extra, and adopters `go get golang.org/x/mobile/bind` in their fresh checkout before running bind. (Until 2026-08-13 the stated reason was that the dep would push the `go` directive past the then-1.22 floor; that floor is now 1.25, so the constraint is dependency hygiene rather than a version conflict.) Android specifically needs `-androidapi 21` because NDK 27 dropped support for API < 21; gomobile's default 16 fails otherwise.
- **Remaining gap (small):** **Typed mobile shared-type API** — if a real mobile adopter wants `Map.SetString` / `Array.PushStringSlice` etc., the gomobile package can be extended with monomorphic typed variants (~200-400 LOC depending on how many type combinations matter). Bytes-only API is sufficient for any adopter willing to do their own protocol-buffer / JSON serialization at the boundary.
- **When to address:** when a real mobile adopter brings a concrete typed-API use case.

## Sync protocol / WebSocket server

### Hocuspocus extensions (resolved)

- **Was:** the full Hocuspocus envelope adds 5 message types beyond the bare y-websocket subset (Auth, Stateless, BroadcastStateless, Close, SyncStatus); `HandleFrame` silently dropped all of them.
- **Resolved by:**
  - `internal/sync/protocol.go` adds AuthSubType constants (PermissionDenied=0, Authenticated=1, Token=2) and `CloseStatusUnauthorized = 4401`.
  - `internal/sync/framing.go` adds encoders + decoders for all 5 types. DecodeEnvelope populates Frame.AuthSub for Auth messages.
  - `internal/sync/handler.go` adds `OnAuthenticate(docName, token) error` and `OnStateless(docName, payload)` hooks plus `AuthFailed` flag. Auth handler decodes Token sub-type, invokes the callback; on deny sends AuthPermissionDenied + Close and sets AuthFailed. Stateless invokes the callback (no reply). BroadcastStateless also fans out via Broadcast. Close and SyncStatus accepted silently.
  - `server/server.go` exposes `Options.OnAuthenticate` and `Options.OnStateless`. The readLoop checks `AuthFailed` after each HandleFrame and tears down the WS with `CloseStatusUnauthorized` (4401) when set.
  - 7 unit tests in `internal/sync/hocuspocus_test.go` plus 3 E2E tests in `server/server_test.go` (deny → 4401 close, accept → Authenticated reply, broadcast-stateless fans out across two clients).
- **Connection-level read-only (resolved):** `server.Options.ReadOnly func(docName, r) bool` marks a connection read-only; the sync handler then drops inbound document mutations (SyncStep2 / SyncUpdate: no apply, no broadcast) while still serving reads (SyncStep1) and still accepting awareness. Enforced in `internal/sync/handler.go` via `Conn.ReadOnly`.
- **Remaining gaps:** finer-grained scoping tied to the auth token specifically (per-document / per-field permissions decided inside `OnAuthenticate` rather than from the request) is still absent; the connection-level `Options.ReadOnly` predicate covers the common viewer-vs-editor case. Add the token-scoped model when an adopter asks.

### Cross-language y-websocket / Hocuspocus fixture (resolved)

- **Was:** byte-level wire format asserted only via hand-built fixtures in `framing_test.go` and pure-Go round-trip in `handler_test.go`.
- **Resolved by:** `testdata/gen/gen-sync.mjs` captures 6 envelope scenarios (SyncStep1 from empty + non-empty doc, SyncStep2 with array state, SyncUpdate incremental text insert, Awareness frame, QueryAwareness handshake) using y-protocols/sync + Hocuspocus outer-tag layout. `internal/sync/fixtures_test.go` decodes each via DecodeEnvelope and verifies Type / SyncSub / Payload match. Reverse-encode test for QueryAwareness (only deterministic-payload type) confirms byte-equality.

### Broadcast fan-out is O(N) per update, without batching (slow consumers resolved)

- **Where:** `server/server.go` `(*conn).broadcast`, `server/sendqueue.go`.
- **Resolved (1.28.0):** the write to each peer no longer runs on the broadcaster's goroutine. Each connection has a bounded send queue and its own writer goroutine; a broadcast only queues, so a slow or stuck client never delays its room, the awareness sweep or the backplane re-broadcast. A client that falls behind (`MaxSendQueue`, `MaxSendQueueBytes`, or a queued message older than `WriteTimeout`) is closed, with 1013 if it still reads (one that does not read is cut by its write deadline), and resyncs when it reconnects. Coalescing a slow client's queued updates into one, instead of closing it, was left out: it costs CPU and allocations on the slow path, only document updates merge, and the resync already recovers the document and presence.
- **Still open:** a broadcast queues one envelope per peer per update, with no batching window, so one fast producer in a very large room costs O(N) queue appends per update. Batch updates within a short window if a workload shows that cost dominating.

### Server.Close leaves live connections open

- **Where:** `server/server.go` `(*Server).Close`.
- **What:** `Close` flushes every document, evicts the idle ones and closes the Backplane, but does not close connections that are still open; they keep editing in memory, and their updates still go to the Store if it is open. `http.Server.Shutdown` does not close them either, because WebSocket connections are hijacked. yserve exits after `Close`, which ends them.
- **When to address:** when an adopter closes a server inside a process that keeps running. Closing them (1001, going away) changes what `Close` does to live clients, so it needs its own release note.

### Persistence: every SyncUpdate is stored as a separate row

- **Where:** `server/server.go` `(*conn).maybePersist` calls `Store.StoreUpdate` per envelope.
- **What:** an active doc with 1000 small edits accumulates 1000 rows in the underlying store before any compaction runs. Flush runs only on the last-disconnect path (see `releaseConn`). For an always-connected doc, the log grows unbounded between server restarts.
- **When to address:** with an in-server auto-flush heuristic. Simple shape: per-doc counter, when N >= 200 schedule a Flush. Pre-condition: lift `Flush` to be safe under concurrent `StoreUpdate` (it already wraps everything in a SQLite transaction, but rest of the API surface should be audited).

### V2 update encoding (resolved)

- **Where:** `internal/encoding/{encoder_v2.go,decoder_v2.go,update_v2.go}`. Public `ygo.{EncodeStateAsUpdateV2,EncodeDiffV2,ApplyUpdateV2}`.
- **Resolved by:** 4 commits over 16 May 2026.
  1. `internal/lib0/rle.go` — RLE primitives (Rle / UintOptRle / IntDiffOptRle / StringEncoder) + 16 unit tests + 16 JS lib0 cross-language fixtures (`7634a53`).
  2. `internal/encoding/encoder_v2.go` + `decoder_v2.go` — column buffer plumbing, 15 round-trip tests (`9104ab8`).
  3. `internal/encoding/update_v2.go` — `Update.EncodeV2` / `Update.DecodeV2` / `ApplyUpdateV2` / `EncodeStateAsUpdateV2` / `EncodeDiffV2` + `EncodeContentV2` / `DecodeContentV2` + V2 DeleteSet diff stream. 10 tests cover Map / Array / Text / DeleteSet / XmlElement / cross-client / incremental diff / V1↔V2 incompat / bad-flag rejection / empty-doc baseline (`3f37287`).
  4. Cross-language V2 fixtures: `testdata/gen/gen-yjs-update-v2.mjs` captures `Y.encodeStateAsUpdateV2` for 29 scenarios (24 mirroring the V1 set + 5 RLE-flexing scenarios that exercise many-keys / large pushes / long strings / monotonic-delta runs). `internal/encoding/v2_fixtures_test.go::TestFixtures_DecodeApplyJSYjsV2Updates` decodes each via `DecodeUpdateV2`, applies via `Update.Apply` (shared with V1), verifies state. All 29 pass under `-race`. CI workflow extended.
- **What still defers:** Hocuspocus V2 sync envelope message codes (4 = SyncStep V2, 5 = SyncDone V2) are not wired into the server; current server uses V1 envelope codes (1/2/3). Adopters needing V2 sync would lift the envelope first. The V2 codec itself is wire-compat-proven in both directions via `cmd/gen-go-fixtures` + `validate-go-fixtures.mjs`.

## Awareness layer

### Cross-language JS y-protocols fixture (resolved)

- **Was:** awareness had only hand-built byte fixtures plus pure-Go round-trip tests; no proof against y-protocols' `encodeAwarenessUpdate` output.
- **Resolved by:** `testdata/gen/gen-awareness.mjs` captures 6 scenarios (name+color, cursor position, two clients, empty state, unicode, set-then-removed); `internal/awareness/fixtures_test.go` decodes each via Apply, verifies state map matches JS via structural JSON equality, plus reverse-direction encode test for single-client scenarios. Wired into CI's fixtures job; runs on every push. Also flagged a JS-impl quirk: `encodeAwarenessUpdate` iterates the caller-supplied client list which is normally `getStates().keys()` (excludes removed), so capturing removal entries requires explicitly passing `meta.keys()` instead.

### Apply returns updated for self-eviction defense bump

- **Where:** `internal/awareness/awareness.go` `Apply` self-eviction branch.
- **What:** when a remote tries to evict the local client and the local clock is bumped instead, we record the local clientID in `Summary.Updated`. yrs records a similar entry in its `AwarenessUpdateSummary::updated` (`awareness.rs:418`); JS records it as part of the regular update flow. The Updated reporting is semantically slightly incorrect — nothing about the local state actually changed except the clock — but matches the reference behaviour.
- **Impact today:** observers see a no-op Update event with the local clientID. Cosmetic only.
- **When to address:** if observers do something behavioural with Updated entries (e.g. re-render UI for the local cursor). Until then, consistency with reference impl wins.

### Resolved: server-driven timeout sweep + presence DoS hardening

- **Was:** `SweepOutdated` was exposed on `internal/awareness/awareness.go` but no goroutine drove it. The awareness type follows yrs (the embedder owns the lifecycle); the y-protocols JS impl runs an internal `setInterval` every `outdatedTimeout/10`. With no driver, a long-lived room accumulated stale presence entries and tombstone keys without bound, and `Apply` accepted unlimited fabricated clientIDs — both memory-exhaustion vectors against an exposed WebSocket server.
- **Resolved by:** the awareness type stays passive (no surprise lifecycle), but the `server/` layer now drives eviction. `server/awareness_sweep.go` runs a ticker at `AwarenessTimeout/10` (floored 1s, matching the JS cadence) that, per live document, `SweepOutdated`s silent clients, broadcasts the removals, then `PurgeTombstones` (a new GC that deletes tombstone keys older than `2*timeout`, reclaiming map slots). Three new caps close the DoS vectors: `decodeUpdate` rejects an entry count above `MaxUpdateEntries` (65536) **before** the count-driven pre-allocation (the length-prefix amplification attack) and a per-entry payload above `MaxStatePayloadBytes` (64 KiB); `Awareness.SetMaxClients(n)` bounds the distinct clientIDs one room tracks (server default 4096, `Options.MaxAwarenessClients`), and `handleAwareness` re-broadcasts only the entries the cap actually accepted (re-encoded from state), so a flood neither bloats the server map nor propagates to cap-less browser peers. Lifecycle is clean: the sweep goroutine starts in `server.New` and is joined in `server.Close`.
- **Remaining:** library callers using `internal/awareness` directly still wrap `SweepOutdated`/`PurgeTombstones` in their own ticker (by design); only the bundled server auto-drives it.

## XML types (mostly resolved)

### Resolved: XmlFragment / XmlElement / XmlText shipped

- **Was:** missing XML wrapper layer; ProseMirror / Tiptap / BlockNote unusable as JS clients against a ygo server.
- **Resolved by:** `internal/types/xml.go` ships `XmlFragment`, `XmlElement`, `XmlText` wrappers. XmlElement carries nodeName + attributes (Map-like via `branch.Map`) + positional children (Array-like via `branch.Start`); XmlText embeds the regular Text wrapper to inherit all rich-text methods. ToString renderers produce HTML-like output with sorted attribute keys. 11 tests cover Element/Attribute/RemoveAttribute, nested DOM round-trip, empty-element render (self-closing until 1.23.0, which writes `<p></p>` as yjs does), rich-text inside XmlText, wire round-trip preserves structure, cross-client structural-edit convergence, Range over children. The wire-format machinery (`ContentType` with optional nodeName) was already in place from the nested-type commit.
- **Remaining:** `XmlHook` (legacy JS Yjs embed type carrying an arbitrary opaque value) is deferred — yrs marks it as legacy too. No adopter has asked.

## Encoding layer

### Resolved: unbounded decode allocation (length-prefix amplification DoS)

- **Was:** the V1/V2 update, snapshot, id-set, state-vector, and Any-content decoders pre-sized slices/maps from a wire-supplied element count (`make([]Block, 0, blockCount)`, `make([]Range, rangeCount)`, `make([]block.Any, count)`, `make(map[string]any, count)`) with no bound. A fuzz harness turned a 9-byte update into a multi-terabyte `make()` — a memory-exhaustion DoS reachable from any peer over the sync wire, since `ApplyUpdate` is the integration entry for untrusted remote changes.
- **Resolved by:** a shared `checkDecodeCount(count, remaining)` guard (`internal/encoding/update.go`) rejects any count exceeding the input bytes available to decode it — every element consumes >= 1 byte, so the bound never rejects a valid encoding (verified: cross-language fixtures pass, and a 3000-block/12 KB update is accepted). Applied at all nine count-driven sites across `update.go`, `update_v2.go` (incl. `DecodeContentV2` and `readDeleteSetV2`), `idset.go`, `state_vector.go`, `any_codec.go`, `content_codec.go`. Found by the new fuzz suite (`*_test.go` `FuzzDecodeUpdate` / `FuzzApplyUpdate` / `FuzzDecodeUpdateV2` / `FuzzDecodeSnapshot` + awareness / sync / relpos targets); crashers are committed as regression corpus under `testdata/fuzz/`, and `.github/workflows/fuzz.yml` fuzzes the decode paths nightly.
- **Remaining:** the V2 block loop can still iterate up to O(input length) on drained column streams (bounded, not amplified) because the RLE column readers return zero values rather than erroring on drain; making them error on drain is a deeper change deferred as low-priority (the count bound already removes the DoS).

## Persistence layer

### GetStateVector replays the full update log every call

- **Where:** `internal/persist/persist.go` `GetStateVector`.
- **What:** the helper builds a fresh `Doc`, replays every stored update through `Update.Apply`, then encodes the resulting state vector. Cost is O(total update bytes) per call. y-leveldb sidesteps this by caching the state vector in a `(docName, "sv")` meta row updated alongside each `StoreUpdate`.
- **Impact today:** correctness is unaffected; large documents (many MB of updates) pay a measurable latency on sync-protocol SV exchanges.
- **When to address:** when sync server lands and SV requests become hot. Either (a) maintain a meta column updated transactionally with every `StoreUpdate`, or (b) auto-flush at N updates so the replay walks a single snapshot.

### No auto-flush trigger

- **Where:** `internal/persist/sqlite/sqlite.go` `(*Store).Flush`.
- **What:** compaction is opt-in. Callers that never call `Flush` accumulate one row per `StoreUpdate` indefinitely. The log stays correct (replay produces converged state) but storage grows linearly with edit count.
- **When to address:** with the sync-server layer, which is the natural place to insert a per-doc heuristic (e.g. "flush after every 100 stored updates"). Until then, library callers either schedule their own periodic flush or accept the growth.

### modernc.org/sqlite dependency pin is fragile — RESOLVED (2026-08-13)

- **Where:** `go.mod`.
- **Was:** to keep `go 1.22` minimum compat we pinned `modernc.org/sqlite v1.29.10` plus its transitive `modernc.org/libc@v1.49.3`, `modernc.org/memory@v1.8.0`, `modernc.org/gc/v3@v3.0.0-20240801135723-a856999a2e4a`, `golang.org/x/sys@v0.30.0`, and told dependabot to ignore newer versions of sqlite and `coder/websocket`. By August 2026 that meant shipping a SQLite build from May 2024 (27 months old) in the layer that stores adopters' documents, to stay compatible with a Go release upstream no longer supports.
- **Resolved by:** raising the module floor to `go 1.25` — the older of the two Go releases upstream still supports — and dropping every pin. `go mod tidy` then picked `modernc.org/sqlite v1.56.0`, `coder/websocket v1.8.15`, `libc v1.74.4`, `memory v1.11.0`, `x/sys v0.47.0`, and removed `gc/v3`, `golang-lru/v2`, `strutil` and `token` from the graph entirely. The dependabot ignore rules are gone, so these track upstream again. Verified across the full suite under `-race`, with the persistence layer additionally run at `-count=10` (WAL and busy-timeout still applied from our DSN; both `BEGIN IMMEDIATE` regression tests still pass).
- **Cost of the fix:** the CI matrix retired `1.22`/`1.23`/`1.24` and is now `1.25`/`1.26`; adopters on those older toolchains stay on ygo v1.15.0.
- **Next step (October 2026):** `modernc.org/sqlite` 1.60 requires Go 1.26, and 1.25 left upstream support when 1.27 shipped, so the floor moved to `go 1.26` with sqlite 1.60.1 (libc v1.77.1, pinned to the version sqlite's own go.mod names) in the nested modules too; the CI matrix is `1.26` and `stable`. Adopters on Go 1.25 stay on ygo v1.22.x.

## Doc / Transaction layer

### TransactionMut.Commit lifecycle (resolved)

- **Was:** Commit only released the write lock; squash, GC, observers, subdocument events and update emission were all missing.
- **Resolved by:** each step as its layer landed: commit-time squash with apply-side partial-overlap slicing (June 2026), GC of deleted content and nested subtrees, observers ordered as yjs fires them (1.21.0), subdocument lifecycle events, and per-transaction update events (`Doc.OnUpdate` / `OnUpdateV2`, 1.22.0). The order is documented on `TransactionMut.Commit` and follows yjs `cleanupTransactions` since October 2026 (entry "Commit-time squash runs before after-transaction handlers").

### GC of deleted nested-type subtrees (resolved)

- **Where:** `internal/doc/transaction.go` `(*TransactionMut).gcDeleted` + `gcNestedChildren`; `internal/store/client_blocks.go` `ReplaceWithGC` + GC-run merge in `SquashFrom`.
- **What it was:** commit-time GC freed the content of a deleted leaf item but left a deleted nested type's children live in the store, byte-diverging from yjs (which collapses the whole subtree). Convergence always held; the gap was a re-encode carrying child items yjs had dropped.
- **Resolved by:** when a deleted item is a `KindType`, `gcDeleted` calls `gcNestedChildren`, which walks the branch's positional children (`Branch.Start` via Right) and keyed children (`Branch.Map` winners plus their overwritten Left-chain), recursing depth-first into nested-of-nested types, and replaces each child cell with a GC cell via `ReplaceWithGC`. The reference item itself still becomes `ContentDeleted`. `SquashFrom` now merges adjacent GC cells into a single run, so the children encode as one ref-0 GC struct, exactly as yjs does. Cross-language fixtures in `nested_gc_test.go` / `testdata/gen/gen-nested-gc.mjs` cover Map-in-Map, Array-in-Map, two-level nesting, and a nested type in an Array (positional parent); all byte-match `yjs@13.6.31`.

### Transaction lifetime not enforced

- **Where:** `internal/doc/transaction.go` `Transaction`, `TransactionMut`.
- **What:** yrs uses a `'doc` lifetime parameter on `Transaction<'doc>` so the borrow checker rejects code that retains a transaction past the doc's lifetime or past the explicit drop. Go has no equivalent. A caller can capture the `*Transaction` returned by `Doc.ReadTxn()` in an outer variable and use it after `Close()`, dereferencing a doc whose lock is no longer held.
- **Why deferred:** runtime checks (e.g. `t.closed` panic on every method) add overhead and noise. The contract is documented in the type doc; mature OSS Go projects (`database/sql.Tx`, `bbolt.Tx`) take the same documentation-only approach.
- **When to address:** if a real bug surfaces that's traceable to retained-after-close. Add a `valid bool` plus panic-on-stale-access; the cost is one branch per method.

### Origin observer dispatch not wired

- **Where:** `internal/doc/transaction.go` `TransactionMut.Origin`.
- **What:** yrs's transactions carry an `Origin` opaque value that observers see in events, used to e.g. tell local edits apart from remote-applied updates (`transaction.rs:1210-1288`). We expose the field but do not yet have an observer subsystem to thread it through.
- **When to address:** with the observer subsystem (paired with the V1 update emit step in the commit lifecycle).

### Subdoc tracking not implemented

- **Where:** `internal/doc/transaction.go` (no `subdocs` field).
- **What:** yrs accumulates `subdocs.added / loaded / removed` sets on TransactionMut and emits `SubdocEvent` at commit. We have no subdoc support at all yet.
- **When to address:** post-MVP; subdocs are an advanced feature, not required for v0.1 scope per DESIGN.md.

### Doc options surface is minimal

- **Where:** `internal/doc/doc.go` `Options`.
- **What:** yrs's `Options` carries auto-load, should-load, GUID, encoder version, collection-id, and other fields. We carry only `DisableGC` and a deterministic `ClientID` override.
- **Why deferred:** none of the other yrs options are reachable from any code path we have ported. Adding them now would be premature surface.
- **When to address:** when porting Doc.load (subdocs) for `AutoLoad`, when porting V2 encoding for `EncoderVersion`, etc. — option-by-option, only when something in the codebase consumes the value.

### ID and Item width: uint64 vs yrs u32

- **Where:** `internal/block/id.go` `ID.Clock uint64`; `internal/block/item.go` `Item.Len uint64`.
- **What:** yrs uses `u32` for clock and len; we use `uint64`. Costs 4 bytes per ID and per Item, accepts wire values up to 2^64-1.
- **Why intentional:** lib0 varuints can carry values up to 64 bits. Yrs would error on a clock above 2^32; we accept it. This is a defensive widening, not a bug.
- **When to revisit:** if memory profiling shows ID storage dominating allocations on large docs. Unlikely.

## Process / tooling

### Local pre-push lint (resolved on this commit)

- **Was:** CI lint kept rejecting pushes because local pre-push only ran `go vet` + tests, not gofmt and not golangci-lint. Two repeat incidents (`5d68d3c` gofmt; `085269d`/`58360cf`/`b1b117f` unused symbols). Local brew installs golangci-lint v2 while CI uses v1.64.8 — configs incompatible.
- **Resolved by:**
  1. CI workflow now pins `version: v1.64.8` (was `latest`). Reproducible.
  2. Root `Makefile` exposes `make check` (gofmt + vet + test + lint) and `make lint-install` (`go install ...@v1.64.8`) for the matching local linter.
- **Remaining:** add a `pre-push` hook installer once collaborators arrive. Tracked separately if needed.

## Benchmarks

### B3.2 (Many clients set Object in shared Map) currently skipped

- **Where:** `benchmarks/b3_test.go` `BenchmarkB3_2_ManyClientsSetObject`.
- **What:** the upstream B3.2 spec writes a JSON `{a:1, b:"x"}` object as the Map value. Our `EncodeAny` does not support `map[string]any` payloads (panics with "unsupported value type"). The same gap blocks any Any TLV containing arrays / objects / buffers / bigint / float32.
- **Why deferred:** Any TLV was scoped to scalars in the MVP — see "Any type is a placeholder" entry above. Closing this unblocks B3.2 (and unlocks objects-as-values for adopters using Map for JSON-shaped configuration).
- **When to address:** with the broader Any TLV variant work. Implementation: extend `internal/encoding/any_codec.go` to handle the upstream lib0 Any tag set (tags 116-127 covering array / object / undefined / float32 / bigint / buffer).

### Cross-implementation benchmark harness (vs yrs / yjs, automated)

- **Where:** missing; tracked here so DESIGN.md's "within 2× of yrs" target stays explicit.
- **What:** [BENCHMARKS.md](/BENCHMARKS.md) "Comparison with yjs / ywasm" section now informally compares ygo's measured numbers against `dmonad/crdt-benchmarks` published yjs/ywasm numbers, with explicit hardware/runtime caveats. That gets us to "qualitatively comparable" but not "definitively prove within 2× under identical conditions". A proper harness would run ygo + native yrs (not ywasm) + yjs through a single comparison runner on the same hardware and produce a comparison table.
- **Why deferred:** upstream `dmonad/crdt-benchmarks` JS runner shells out to native modules; integrating ygo would need a Go-side adapter producing the same JSON output the JS aggregator consumes. Native yrs needs the rust toolchain present; CI overhead non-trivial. Marginal value over the informal comparison.
- **When to address:** opportunistic; current absolute numbers + qualitative comparison are sufficient for grant applications and project positioning. Re-prioritise if a reviewer / adopter specifically asks "show me side-by-side on identical hardware".

## Client / mobile / server layers (added 2026-06-12 sprint)

### gomobile bind is not run in CI

- **Where:** `gomobile/` package; verification noted in `gomobile.go` package doc and README.
- **What:** `gomobile bind -target=ios/android` was manually verified once (2026-06-12) to produce a valid xcframework / AAR, but the bind step is not part of CI (it needs Xcode / Android NDK). CI only compiles the package under pure Go, which guards against an inadvertent CGO leak via a dependency but does not prove the bind output stays valid.
- **Why deferred:** running gomobile bind in CI needs macOS + Xcode + NDK runners, heavy and slow for a binding whose shape rarely changes.
- **When to address:** if a binding regression ships unnoticed, or when a CI provider with the toolchain is cheap to add. Until then, re-run the README commands manually before any release that touches `gomobile/`.

### gomobile Map is string-keys / string-values only

- **Where:** `gomobile/types.go` `Map.SetString` / `GetString`.
- **What:** the bindable Map exposes only string values. Non-string values written by JS peers (numbers, nested types, embeds) read back as the empty string rather than a typed value, because `any` cannot cross the gomobile bind boundary.
- **Why deferred:** a typed-value bridge needs a tagged-union encoding (e.g. JSON per value) that adopters can decode on the UI side; out of scope for the first app-level SDK cut.
- **When to address:** when a mobile adopter needs to read non-string map values; add `GetJSON(key)` returning the value as a JSON blob, mirroring the bytes-level approach.

### gomobile awareness can only encode all clients

- **Where:** `gomobile/gomobile.go` `Awareness.Encode` (package-doc note).
- **What:** the per-client `[]uint64` selector of `internal/awareness.Encode` cannot cross the bind boundary, so the mobile wrapper exposes only encode-all. Fine for the common "broadcast my one entry" path the Client uses, but a caller wanting to encode a specific subset cannot.
- **When to address:** if a mobile adopter needs selective awareness encoding; add a single-ID convenience method.

### client offline persistence: delete-set redundancy and disk-full durability

- **Where:** `client/persist.go` (`persistNow`).
- **What:** the offline-first local store persists `EncodeDiff(doc, watermark)` on each change, which always re-emits the FULL delete set. For a delete-heavy document each persisted blob redundantly carries every prior deletion, so the log grows faster than the new content alone. Mitigated by `Flush` compaction every 64 writes and on Close; replay is idempotent, so it never corrupts state, only bloats the log between flushes.
- **Also:** on a `StoreUpdate` failure (e.g. disk full) mid-session the loop reports the error (via `OnError`, else a log line) and schedules one paced retry; but if the FINAL edit before shutdown fails to write and the retry also fails, that edit is absent from the local store (prior persisted state is intact, since `StoreUpdate` is atomic-on-error). This is a durability edge, not a replay-correctness bug.
- **Why deferred:** an exact per-transaction incremental encoder (new structs + only THIS txn's deletions, not the whole delete set) would remove the redundancy; a bounded retry queue with persistent backoff would harden the disk-full edge. Both are more machinery than the current sqlite-on-device target warrants.
- **When to address:** if a profiled mobile workload shows log growth between flushes, or if a durability-critical deployment needs guaranteed last-write persistence (then add a write-ahead acknowledgement).

### server auto-versioning assumes a single writer

- **Where:** `server/versioning.go` (dirty-set sweep).
- **What:** auto-versioning marks a document dirty on each persisted update from THIS server process, then versions dirty documents on a timer. A document written by a different process sharing the same database (multi-node / external writer) is not observed and would be under-versioned.
- **Why deferred:** the single-binary deployment yserve targets is single-writer by design (SQLite file lock). Multi-writer versioning needs a storage-level change feed.
- **When to address:** when a clustered / multi-writer deployment story lands (it would also need the Redis-relay path we do not yet have).

## Open questions captured but not resolved

The following are flagged in `docs/yrs-port-notes/block.md` § "Open questions" — re-read at the time the relevant code is touched:

1. **`BlockRange::last_id` off-by-one wart in yrs** (`clock + len` vs `Item::last_id`'s `clock + len - 1`). Affects skip blocks / delete set encoding; verify against Yjs wire format when porting the encoder.
2. **`ITEM_FLAG_MARKED` "not used atm"** — kept as a constant for bit-position equivalence with future Yjs versions; before declaring it dead, grep yrs for usage.
3. **`small-client` feature in yrs** (32-bit ClientID) — we target 53-bit ClientIDs always; ignore this code path.
4. **`Item::PartialEq` semantics** — Go `EqualByID` matches by ID only, mirroring how yrs's derived equality reduces through `ItemPtr::PartialEq`. If full structural equality (content + flags + neighbours) is needed for tests, add a separate `EqualDeep` helper rather than overloading `EqualByID`.
5. **Format-attribute integration is a stub in yrs** (`@todo searchmarker`). For rich text we may need to consult upstream Yjs directly rather than trust yrs's path.

## How this file evolves

- Resolve an item: delete the entry. The git history preserves it. Do not strike-through; keep the file short.
- New deferral: add an entry. Always include the four headings (Where / What / Why / When to address) plus a Reference if upstream code or docs exist.
- A deferral that ages past 6 months without resolution becomes a candidate for either resolving or formally moving to "out of scope" status in DESIGN.md.
