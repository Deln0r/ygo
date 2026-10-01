// Package undo implements the Yjs UndoManager semantics in pure Go.
//
// An UndoManager subscribes to a Doc's AfterTransaction lifecycle and
// records each qualifying write as a StackItem on its undo stack.
// Undo / Redo replay those StackItems in reverse / forward direction,
// using a redone chain on Item to resurrect deletions without violating
// the YATA-style "IDs are immutable" invariant.
//
// Scope-and-origin filtering: a transaction qualifies only if at least
// one of its changedTypes lies under the configured scope (root branch
// match in this first cut; nested-type ancestry is planned) AND its
// Origin is in the configured trackedOrigins set. The default
// trackedOrigins is {nil}, which matches local edits made without an
// explicit origin, and also updates applied without one (yjs behaves
// the same): apply a peer's updates with an origin to keep them out of
// the undo history.
//
// Capture-timeout grouping: bursty edits within captureTimeout collapse
// into a single StackItem so a single Undo restores a recognisable unit
// of work. StopCapturing forces the next change to open a fresh
// StackItem regardless of timing.
//
// Wire format: UndoManager state is local. There is no wire encoding
// for the undo or redo stacks. Two replicas each running their own
// UndoManager observe independent histories.
//
// See docs/undo-manager-design.md for the full design rationale and
// the ship plan; this package is grown commit-by-commit against that
// plan.
package undo
