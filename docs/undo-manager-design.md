# UndoManager design

**Status: shipped (Map + Array + Text), 2026-06-08.** Sequence (Array/Text) resurrection, the top-level `ygo.NewUndoManager` API, and cross-language conformance fixtures vs `yjs@13.6.31` (7 scenarios) are all in. Deferred to follow-ups: nested-type parent resurrection (recursive parent-redo), `Meta` selection payload population, `ignoreRemoteMapChanges` corner cases, custom `deleteFilter` / `captureTransaction` callbacks.

Design notes for `internal/undo`. Mirrors the upstream `yjs@13.6.x` UndoManager semantics, and uses the existing `internal/encoding.IdSet` as the underlying DeleteSet primitive.

## Why this is non-trivial

A CRDT undo cannot mutate or reuse historical IDs. Restoring a deleted item means inserting a fresh item with a new (clientID, clock) pair and linking it back to the deleted ancestor via a `redone` chain. The chain matters because subsequent undo / redo cycles need to find the latest live representation of the original change.

The grouping of rapid edits into a single "undo step" (the `captureTimeout` mechanism) is a separate axis. It compresses bursty typing into one StackItem so a single undo restores a recognisable unit of work for the user.

## Scope and intentional non-goals

In scope for the first cut:

- Track local-origin mutations only (default `trackedOrigins = {nil}`). Note added in 1.22.0: `{nil}` also matches updates applied without an origin, exactly as yjs's default `{null}` does, so remote updates belong under an origin of their own (`ApplyUpdateWithOrigin`); `AddTrackedOrigin` / `RemoveTrackedOrigin` change the set after construction.
- Track configurable scope: a slice of root branches (typically the Map / Array / Text the application cares about)
- Two stacks: undo and redo
- Capture-timeout grouping (default 500 ms)
- StackItem holds two IdSets: `insertions` (what to delete on undo) and `deletions` (what to restore on undo)
- Undo / Redo as in-place transactions; emit `stack-item-popped` semantically (Go-idiomatic observer to be designed)
- Restoration via `redone` chain on `block.Item` (the YATA-style "this item is the redone version of that older deleted item")

Out of scope for the first cut, deferred to a later patch:

- Cross-client undo coordination (yjs treats UndoManager as a local-only concept; we do too)
- Selection metadata stored in StackItem.meta (yjs has it for editor integration; we expose hooks but the first cut does not populate)
- `ignoreRemoteMapChanges` corner cases
- Custom `deleteFilter` and `captureTransaction` callbacks (we keep the names in the options struct but stub out as accept-all for now)

## Public API shape

In the top-level `ygo` package:

```go
type UndoManager struct {
    // exported by getters; underlying state in internal/undo
}

type UndoManagerOptions struct {
    CaptureTimeout time.Duration       // default 500 * time.Millisecond
    TrackedOrigins map[any]struct{}    // default {nil}: untagged local edits and untagged applies
    IgnoreRemoteMapChanges bool        // default false
}

func NewUndoManager(scope []Branch, opts ...UndoManagerOptions) *UndoManager

func (um *UndoManager) Undo() bool              // true if a stack item was applied
func (um *UndoManager) Redo() bool
func (um *UndoManager) CanUndo() bool
func (um *UndoManager) CanRedo() bool
func (um *UndoManager) StopCapturing()          // prevents grouping with the next change
func (um *UndoManager) Clear()                  // empties both stacks
func (um *UndoManager) Close()                  // unregisters the doc-level handler
```

`Branch` is already the top-level type alias in `ygo.go` over `block.Branch`. `scope` is "the roots this UndoManager cares about"; mutations under any of these (or any of their descendants for nested types) qualify.

## Internal mechanics

### Observer hook on Doc

We add a small subscription primitive to `internal/doc.Doc`:

```go
type AfterTransactionHandler func(*TransactionMut)

func (d *Doc) OnAfterTransaction(fn AfterTransactionHandler) (unsubscribe func())
```

The slice of handlers is stored on `Doc` behind the existing `mu`. `TransactionMut.Commit` invokes each handler after marking the transaction closed but before unlocking the doc mutex. Handler order is registration order; panics in handlers must not leave the lock held.

### Transaction state we need to capture

`TransactionMut` already tracks `deletedIDs` and `changedTypes`. To support the StackItem model we also need:

- `beforeState map[uint64]uint64` — clock head per client at the moment WriteTxn was acquired
- `afterState map[uint64]uint64` — clock head per client at Commit time
- `deleteSet *encoding.IdSet` — the canonical IdSet form of deletions during this transaction; built from `deletedIDs` at Commit time if we want to keep deletedIDs as a simple slice

These three become accessible to AfterTransaction handlers. The insertions IdSet for a StackItem is derived as `afterState - beforeState` per client.

### StackItem

```go
type StackItem struct {
    Insertions *encoding.IdSet  // items inserted during this captured window
    Deletions  *encoding.IdSet  // items deleted during this captured window
    Meta       map[string]any   // user-supplied metadata; unused by core logic
}
```

Internally the undo package keeps `Insertions` and `Deletions` populated via the StackItem-builder loop in the AfterTransaction handler. Both fields are merged on a follow-up transaction if it lands within `CaptureTimeout` of the previous one.

### The `redone` chain on Item

`block.Item` gets one optional field, the ID of the item that this item replaces in a redo operation:

```go
type Item struct {
    // ... existing fields ...
    Redone *ID                // points to the deleted item this item restores; nil if not a redo
}
```

A helper, `followRedone(store, id)`, walks the chain from a deleted ancestor's ID to the latest live version. yjs implements this lazily; we do the same.

`Item.Redone` does NOT cross the wire. It is local-only state used by the UndoManager to find the right item on the next Undo / Redo.

### Capture grouping logic

The AfterTransaction handler decides between (a) appending to the existing top StackItem and (b) pushing a fresh StackItem onto the stack. Conditions for append:

- the previous StackItem exists,
- `time.Since(lastChange) < captureTimeout`,
- not currently in `undoing` or `redoing` state.

`stopCapturing()` resets `lastChange` to a sentinel that always fails the time check, so the next change starts a fresh StackItem.

### Undo / Redo execution

Both operations follow the same flow with different stacks:

1. Acquire WriteTxn with `Origin = um` (so the resulting after-transaction does not feed back into the same stack)
2. Pop the top StackItem
3. Walk `stackItem.Insertions`: for every Item ID range covered, delete the item via `TransactionMut.Delete`
4. Walk `stackItem.Deletions`: for every Item ID range covered, run `redoItem` to either resurrect-via-redone-chain or insert a fresh item linked back via `Redone`
5. Set the `undoing` (or `redoing`) flag on the UndoManager so the AfterTransaction handler routes the resulting StackItem to the opposite stack

`redoItem` is the core restoration primitive. For an Item targeted by `Deletions`, it either follows an existing `Redone` chain (if the deleted item was already restored once and re-deleted) or creates a new Item with the same `Content`, the same `ParentSub`, the same logical position (relative to its `Origin` / `RightOrigin` neighbours), a fresh ID `(localClientID, nextClock)`, and `Redone = &deletedItem.ID`.

`getItemCleanStart` (yjs helper) becomes a TransactionMut method that materialises the item exactly at a given start ID, splitting blocks as needed. We have `MaterializeCleanStart` already; this is the same primitive.

## Cross-language compatibility note

UndoManager state is local-only in upstream yjs. There is no wire format for the undo stacks. Two clients each running UndoManager observe independent histories; an undo on client A only reverses changes A made.

This means our cross-language fixture coverage for UndoManager is narrower than for the wire-format work. We test:

- Pure-Go scenarios: Map / Array / Text operations followed by Undo and Redo, asserting final state and that the resulting wire updates are byte-identical to what a yjs UndoManager would have produced from the same operation sequence.
- Round-trip scenarios: Go does the ops, encodes the resulting update, JS yjs decodes and applies; the resulting yjs doc state matches.

We do not test "JS does Undo, Go observes the resulting wire bytes" round-trip directly because the only thing crossing the wire is the result of undo (which is just a deletion or insertion update) and our existing 114-scenario suite already validates those wire forms.

## File layout

```
internal/undo/
    doc.go               package doc comment
    undo.go              UndoManager type, options, capture logic
    stack_item.go        StackItem type and merge helpers
    redo_item.go         redoItem function and followRedone helper
    capture_test.go      grouping-timeout unit tests
    undo_redo_test.go    scenario tests
```

Top-level `ygo.go` adds:

```go
type UndoManager = undo.UndoManager
type UndoManagerOptions = undo.Options
type StackItem = undo.StackItem

func NewUndoManager(scope []Branch, opts ...UndoManagerOptions) *UndoManager {
    return undo.NewUndoManager(scope, opts...)
}
```

## Ship plan

1. Observer infrastructure on Doc and Commit dispatch (this commit + next)
2. `beforeState` / `afterState` / `deleteSet` populated on TransactionMut
3. `internal/undo` skeleton with StackItem, options, capture logic
4. `Redone` field on Item + `redoItem` + `followRedone`
5. Undo / Redo execution
6. Unit tests across Map / Array / Text including nested
7. Top-level API wiring and README update
8. Cross-language fixture extension and verification against `yjs@13.6.31`

Each step a separate commit on `main`.

## References

- yjs reference: `src/utils/UndoManager.js` (396 LOC)
- yrs reference: `yrs/src/undo.rs` (extensive)
- Standard pattern: YATA-with-redone (Petrescu 2016)
