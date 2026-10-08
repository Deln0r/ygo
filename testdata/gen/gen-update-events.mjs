// Generates testdata/update-event-fixtures.json - the bytes yjs@13.6.33 emits
// per transaction on doc.on('update') and doc.on('updateV2'), and the bytes of
// Y.encodeStateAsUpdate / Y.encodeStateAsUpdateV2 against state vectors that
// end inside a block.
//
// Each scenario runs steps over one or two documents with fixed client IDs.
// A step is either a transaction of edits on one document (with an origin) or
// an update from one document applied to another (with an origin). Every
// 'update' / 'updateV2' event of every document is recorded in order, with
// the step that caused it; a step that changes nothing records no event. For
// an apply step the exact bytes applied are recorded too, so the Go test
// applies the same bytes. Scenarios marked with diffs also record, for the
// final state of one document (0 unless diffs_doc says otherwise), the V1
// and V2 diff against every state vector in a grid over each client's
// clocks.
//
// The Go test (update_event_fixture_test.go) replays the steps through ygo's
// public API and compares every event, origin and diff byte for byte.
//
// Values are JSON. Integers stay integers (lib0 writes them as varints), so
// the Go side decodes JSON numbers as int64 when they are integral.
//
// Run: node gen-update-events.mjs (after npm install in this directory).

import { writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";
import * as Y from "yjs";

const here = dirname(fileURLToPath(import.meta.url));
const outPath = resolve(here, "..", "update-event-fixtures.json");

// Client IDs. Large enough that their varints take several bytes, and each
// doc sorts above the next, so the descending client order in an update is
// visible.
const CLIENTS = [3141592653, 271828182, 141421356, 57721566];

// Ops. `path` locates a shared type: "<kind>:<name>" for a root (text, array,
// map), then map keys (strings) and array indices (numbers) for nested types.
const ins = (path, index, text, attrs) => ({ op: "insert", path, index, text, ...(attrs ? { attrs } : {}) });
const del = (path, index, len) => ({ op: "delete", path, index, len });
const fmt = (path, index, len, attrs) => ({ op: "format", path, index, len, attrs });
const embed = (path, index, value) => ({ op: "embed", path, index, value });
const push = (path, values) => ({ op: "push", path, values });
const insv = (path, index, values) => ({ op: "insertValues", path, index, values });
const set = (path, key, value) => ({ op: "set", path, key, value });
const setType = (path, key, kind) => ({ op: "setType", path, key, kind });
const insType = (path, index, kind) => ({ op: "insertType", path, index, kind });
const mdel = (path, key) => ({ op: "mapDelete", path, key });
const mclear = (path) => ({ op: "mapClear", path });

// Steps.
const txn = (doc, origin, ...ops) => ({ doc, origin, ops });
// what: "state" applies the full state of `from`; "diff" applies the diff
// against the current state vector of `to`; "since" applies the diff against
// an explicit state vector `sv` ({client: clock}).
const apply = (from, to, origin, what, sv) => ({ apply: { from, to, origin, what, ...(sv ? { sv } : {}) } });
// replay applies again the exact bytes an earlier apply step sent.
const replay = (step, to, origin) => ({ apply: { replay: step, to, origin } });
const undo = (doc, um) => ({ undo: { doc, ...(um !== undefined ? { um } : {}) } });
const redo = (doc, um) => ({ redo: { doc, ...(um !== undefined ? { um } : {}) } });
// manage creates manager k of sc.managers ({doc, scope, origins, group}) at
// that point, so a nested scope exists by then; clear empties its stacks.
const manage = (k) => ({ manage: k });
const clear = (doc, um) => ({ clear: { doc, um } });
const stop = (doc, um) => ({ stop: { doc, um } });
// XML ops.
const xmlElement = (path, index, name) => ({ op: "xmlElement", path, index, name });
const xmlText = (path, index) => ({ op: "xmlText", path, index });
const setAttr = (path, key, value) => ({ op: "setAttr", path, key, value });

const T = "text:t";
const A = "array:a";
const M = "map:m";
const X = "xml:x";
const O = "map:o";

const appends = [];
for (let i = 0; i < 200; i++) appends.push(txn(0, null, ins(T, i, "x")));

const scenarios = [
  {
    name: "append-200",
    description: "200 one-character appends by one writer, one transaction each: every event carries one character",
    steps: appends,
  },
  {
    name: "append-words",
    description: "appends of several characters each, merged into one block by the end",
    steps: [txn(0, null, ins(T, 0, "hello")), txn(0, null, ins(T, 5, " ")), txn(0, null, ins(T, 6, "world"))],
    diffs: true,
  },
  {
    name: "insert-middle",
    description: "an insert inside an existing block splits it",
    steps: [txn(0, null, ins(T, 0, "abc")), txn(0, null, ins(T, 1, "X")), txn(0, null, ins(T, 0, "Y"))],
    diffs: true,
  },
  {
    name: "delete-range",
    description: "a delete inside one block",
    steps: [txn(0, null, ins(T, 0, "hello world")), txn(0, null, del(T, 2, 5))],
    diffs: true,
  },
  {
    name: "delete-across-blocks",
    description: "a delete spanning a split block and an insert inside it",
    steps: [
      txn(0, null, ins(T, 0, "abc")),
      txn(0, null, ins(T, 3, "def")),
      txn(0, null, ins(T, 1, "X")),
      txn(0, null, del(T, 0, 5)),
    ],
    diffs: true,
  },
  {
    name: "insert-and-delete-in-one-transaction",
    description: "text inserted and partly deleted in the same transaction, garbage collection on",
    steps: [txn(0, null, ins(T, 0, "abcdef"), del(T, 1, 2)), txn(0, null, ins(T, 4, "g"))],
    diffs: true,
  },
  {
    name: "insert-and-delete-in-one-transaction-no-gc",
    description: "the same with garbage collection off: the deleted text stays in the update",
    gc: false,
    steps: [txn(0, null, ins(T, 0, "abcdef"), del(T, 1, 2)), txn(0, null, ins(T, 4, "g"))],
    diffs: true,
  },
  {
    name: "delete-then-insert-at-same-place",
    description: "one transaction deletes a character and inserts another where it was",
    steps: [txn(0, null, ins(T, 0, "abc")), txn(0, null, del(T, 1, 1), ins(T, 1, "Y"))],
  },
  {
    name: "delete-twice",
    description: "two deletes in one transaction, merged into one delete-set range per client",
    steps: [txn(0, null, ins(T, 0, "abcdefgh")), txn(0, null, del(T, 5, 2), del(T, 1, 2), del(T, 1, 2))],
  },
  {
    name: "format-plain-text",
    description: "formatting a range of plain text, then removing part of it, then appending",
    steps: [
      txn(0, null, ins(T, 0, "hello world")),
      txn(0, null, fmt(T, 0, 5, { bold: true })),
      txn(0, null, fmt(T, 2, 2, { bold: null })),
      txn(0, null, ins(T, 11, "!")),
    ],
    diffs: true,
  },
  {
    name: "embed",
    description: "an embed between appends",
    steps: [txn(0, null, ins(T, 0, "ab")), txn(0, null, embed(T, 2, { image: "x.png" })), txn(0, null, ins(T, 3, "c"))],
    diffs: true,
  },
  {
    name: "emoji-append",
    description: "an append after a surrogate pair; diffs include a state vector inside the pair",
    steps: [txn(0, null, ins(T, 0, "a\u{1F600}")), txn(0, null, ins(T, 3, "b")), txn(0, null, ins(T, 4, "\u{1F680}c"))],
    diffs: true,
  },
  {
    name: "map-overwrite",
    description: "a map key written three times and deleted",
    steps: [
      txn(0, null, set(M, "k", 1)),
      txn(0, null, set(M, "k", "two")),
      txn(0, null, set(M, "k", { three: 3 })),
      txn(0, null, mdel(M, "k")),
    ],
    diffs: true,
  },
  {
    name: "map-keys-in-one-transaction",
    description: "several keys in one transaction, then one of them again",
    steps: [txn(0, null, set(M, "a", true), set(M, "b", null), set(M, "c", 2.5)), txn(0, null, set(M, "b", [1, "x"]))],
  },
  {
    name: "array-push-merge",
    description: "pushes that merge into one block, then an insert and a delete inside it",
    steps: [
      txn(0, null, push(A, [1, 2, 3])),
      txn(0, null, push(A, [4])),
      txn(0, null, push(A, ["s", true, null, { o: 1 }])),
      txn(0, null, insv(A, 2, [9])),
      txn(0, null, del(A, 1, 3)),
    ],
    diffs: true,
  },
  {
    name: "nested-map",
    description: "a nested map created, then written twice",
    steps: [txn(0, null, setType(M, "child", "map")), txn(0, null, set([M, "child"], "k", "v")), txn(0, null, set([M, "child"], "k", "w"))],
    diffs: true,
  },
  {
    name: "nested-text-in-array",
    description: "a text inside an array, appended to in separate transactions",
    steps: [txn(0, null, insType(A, 0, "text")), txn(0, null, ins([A, 0], 0, "ab")), txn(0, null, ins([A, 0], 2, "cd"))],
    diffs: true,
  },
  {
    name: "nested-type-deleted",
    description: "deleting a map that holds live keys: yjs deletes the keys too, so they are in the transaction's delete set",
    steps: [
      txn(0, null, setType(M, "child", "map")),
      txn(0, null, set([M, "child"], "a", 1), set([M, "child"], "b", 2)),
      txn(0, null, mdel(M, "child")),
    ],
  },
  {
    name: "nested-array-collected",
    description: "a deleted nested array becomes a garbage-collected run; diffs include state vectors inside the run",
    steps: [txn(0, null, setType(M, "arr", "array")), txn(0, null, push([M, "arr"], [1, 2, 3, 4, 5])), txn(0, null, mdel(M, "arr")), txn(0, null, set(M, "after", 1))],
    diffs: true,
  },
  {
    name: "empty-transaction",
    description: "a transaction with no edits, and one whose only edit changes nothing, emit no event",
    steps: [txn(0, null, ins(T, 0, "a")), txn(0, null), txn(0, null, ins(T, 1, "")), txn(0, null, ins(T, 1, "b"))],
  },
  {
    name: "origins",
    description: "the event carries the transaction's origin",
    steps: [txn(0, "local", ins(T, 0, "a")), txn(0, null, ins(T, 1, "b")), txn(0, "other", ins(T, 2, "c"))],
  },
  {
    name: "remote-apply",
    description: "a peer's edits applied with an origin; the receiver's event holds what it integrated",
    docs: 2,
    steps: [
      txn(1, null, ins(T, 0, "abc")),
      apply(1, 0, "remote", "state"),
      txn(0, null, ins(T, 3, "def")),
      apply(0, 1, "remote", "diff"),
      txn(1, null, del(T, 1, 3)),
      apply(1, 0, "remote", "diff"),
    ],
    diffs: true,
  },
  {
    name: "remote-partly-known",
    description: "a peer's full state applied again after it grew: the receiver integrates only the new tail of a merged block",
    docs: 2,
    steps: [
      txn(1, null, ins(T, 0, "abc")),
      apply(1, 0, "remote", "state"),
      txn(1, null, ins(T, 3, "def")),
      apply(1, 0, "remote", "state"),
      apply(1, 0, "remote", "state"),
      txn(1, null, ins(T, 6, "gh")),
      apply(1, 0, "remote", "since", { [CLIENTS[1]]: 4 }),
    ],
  },
  {
    name: "concurrent-writers",
    description: "two writers editing the same text concurrently, synced both ways; diffs over both clients",
    docs: 2,
    steps: [
      txn(0, null, ins(T, 0, "base")),
      apply(0, 1, "remote", "state"),
      txn(0, null, ins(T, 4, "-zero")),
      txn(1, null, ins(T, 0, "one-")),
      txn(1, null, ins(T, 2, "X")),
      apply(1, 0, "remote", "diff"),
      apply(0, 1, "remote", "diff"),
      txn(0, null, del(T, 1, 6)),
      apply(0, 1, "remote", "diff"),
    ],
    diffs: true,
  },
  {
    name: "delete-set-is-the-transactions-own",
    description: "an append after a delete carries no delete set; a later delete carries only its own range",
    steps: [txn(0, null, ins(T, 0, "abc")), txn(0, null, del(T, 0, 1)), txn(0, null, ins(T, 2, "d")), txn(0, null, del(T, 0, 1))],
  },
  {
    name: "remote-deleted-content",
    description: "a peer's full state carrying deleted content: the receiver's delete set holds that range",
    docs: 2,
    steps: [txn(0, null, ins(T, 0, "abc")), txn(0, null, del(T, 1, 1)), apply(0, 1, "remote", "state")],
  },
  {
    name: "remote-deleted-content-partly-known",
    description: "the receiver has the start of a block; the peer appends to it, deletes all of it and sends its state: the deleted struct is cut at the receiver's clock",
    docs: 2,
    steps: [
      txn(0, null, ins(T, 0, "abc")),
      apply(0, 1, "remote", "state"),
      txn(0, null, ins(T, 3, "def")),
      txn(0, null, del(T, 0, 6)),
      apply(0, 1, "remote", "state"),
    ],
    diffs: true,
    diffsDoc: 1,
  },
  {
    name: "remote-collected-run-partly-known",
    description: "a peer's garbage-collected run whose start the receiver already has: the unknown tail must integrate",
    docs: 2,
    steps: [
      txn(0, null, setType(M, "p", "text")),
      txn(0, null, ins([M, "p"], 0, "a")),
      apply(0, 1, "remote", "state"),
      txn(0, null, ins([M, "p"], 1, "bc")),
      txn(0, null, mdel(M, "p")),
      apply(0, 1, "remote", "state"),
    ],
  },
  {
    name: "remote-write-into-collected-parent",
    description: "a peer writes into a nested map another peer deleted concurrently: the write integrates as a collected run",
    docs: 2,
    steps: [
      txn(0, null, setType(M, "p", "map")),
      apply(0, 1, "remote", "state"),
      txn(0, null, mdel(M, "p")),
      txn(1, null, set([M, "p"], "x", 1)),
      apply(1, 0, "remote", "diff"),
      txn(1, null, set(M, "later", 2)),
      apply(1, 0, "remote", "diff"),
    ],
  },
  {
    name: "concurrent-formatting",
    description: "two peers bold the same text concurrently; yjs follows the remote update with a cleanup transaction of its own",
    docs: 2,
    steps: [
      txn(0, null, ins(T, 0, "xy")),
      apply(0, 1, "remote", "state"),
      txn(0, null, fmt(T, 0, 2, { bold: true })),
      txn(1, null, fmt(T, 0, 2, { bold: true })),
      apply(1, 0, "remote", "diff"),
      apply(0, 1, "remote", "diff"),
    ],
  },
  {
    name: "deleted-runs-merge-without-gc",
    description: "with garbage collection off, adjacent deleted pieces of one block merge back into one struct",
    gc: false,
    steps: [txn(0, null, ins(T, 0, "abc")), txn(0, null, del(T, 1, 1)), txn(0, null, del(T, 0, 2)), txn(0, null, ins(T, 0, "d"))],
    diffs: true,
  },
  {
    name: "replayed-delete",
    description: "an old update deleting part of a block, delivered again after the rest was deleted: nothing changes",
    docs: 2,
    steps: [
      txn(0, null, ins(T, 0, "abc")),
      apply(0, 1, "remote", "state"),
      txn(0, null, del(T, 1, 1)),
      apply(0, 1, "remote", "state"),
      txn(0, null, del(T, 0, 2)),
      apply(0, 1, "remote", "state"),
      apply(0, 1, "remote", "since", { [CLIENTS[0]]: 0 }),
      replay(3, 1, "remote"),
      txn(1, null, ins(T, 0, "z")),
    ],
    diffs: true,
    diffsDoc: 1,
  },
  {
    name: "undo-grouped-deletes",
    description: "text written before the UndoManager, deleted one character per transaction in one undo step, then undone",
    steps: [txn(0, "setup", ins(T, 0, "ab")), txn(0, null, del(T, 1, 1)), txn(0, null, del(T, 0, 1)), undo(0)],
    undoScope: T,
    undoGroup: true,
  },
  {
    name: "undo-nested-map-delete",
    description: "a nested map with keys deleted, undone (it comes back with its keys), redone, undone again",
    managers: [{ doc: 0, scope: M }],
    states: true,
    steps: [
      txn(0, "setup", setType(M, "child", "map")),
      txn(0, "setup", set([M, "child"], "a", 1), set([M, "child"], "b", "x")),
      manage(0),
      txn(0, null, mdel(M, "child")),
      undo(0, 0),
      redo(0, 0),
      undo(0, 0),
    ],
    diffs: true,
  },
  {
    name: "undo-nested-array-delete",
    description: "a nested array of two blocks deleted and undone",
    managers: [{ doc: 0, scope: M }],
    states: true,
    steps: [
      txn(0, "setup", setType(M, "arr", "array")),
      txn(0, "setup", push([M, "arr"], [1, 2])),
      txn(0, "setup", push([M, "arr"], ["three"])),
      manage(0),
      txn(0, null, mdel(M, "arr")),
      undo(0, 0),
      redo(0, 0),
    ],
  },
  {
    name: "undo-nested-formatted-text-delete",
    description: "a nested text with a bold run deleted and undone: the format markers come back too",
    managers: [{ doc: 0, scope: M }],
    states: true,
    steps: [
      txn(0, "setup", setType(M, "t", "text")),
      txn(0, "setup", ins([M, "t"], 0, "hello")),
      txn(0, "setup", fmt([M, "t"], 0, 2, { bold: true })),
      manage(0),
      txn(0, null, mdel(M, "t")),
      undo(0, 0),
      redo(0, 0),
    ],
  },
  {
    name: "undo-three-levels-delete",
    description: "a map in a map in a map, holding a text, deleted from the top and undone",
    managers: [{ doc: 0, scope: M }],
    states: true,
    steps: [
      txn(0, "setup", setType(M, "a", "map")),
      txn(0, "setup", setType([M, "a"], "b", "map")),
      txn(0, "setup", set([M, "a", "b"], "c", 1), setType([M, "a", "b"], "d", "text")),
      txn(0, "setup", ins([M, "a", "b", "d"], 0, "deep")),
      manage(0),
      txn(0, null, mdel(M, "a")),
      undo(0, 0),
      redo(0, 0),
      undo(0, 0),
    ],
    diffs: true,
  },
  {
    name: "undo-types-in-array-delete",
    description: "a map and a text inside an array deleted together and undone",
    managers: [{ doc: 0, scope: A }],
    states: true,
    steps: [
      txn(0, "setup", insType(A, 0, "map")),
      txn(0, "setup", set([A, 0], "k", "v"), insType(A, 1, "text")),
      txn(0, "setup", ins([A, 1], 0, "tx")),
      manage(0),
      txn(0, null, del(A, 0, 2)),
      undo(0, 0),
      redo(0, 0),
    ],
  },
  {
    name: "undo-keeps-ancestors",
    description: "a tracked delete inside a map keeps the map: an untracked delete of the map then leaves its content uncollected",
    managers: [{ doc: 0, scope: M }],
    states: true,
    steps: [
      txn(0, "setup", setType(M, "child", "map")),
      txn(0, "setup", set([M, "child"], "a", 1)),
      manage(0),
      txn(0, null, mdel([M, "child"], "a")),
      txn(0, "other", mdel(M, "child")),
      undo(0, 0),
    ],
    diffs: true,
  },
  {
    name: "undo-child-scope-parent-deleted",
    description: "a manager scoped to a nested map ignores the map's deletion, even though deleting it deletes a live key inside: its redo history survives",
    managers: [{ doc: 0, scope: [M, "child"] }],
    states: true,
    steps: [
      txn(0, "setup", setType(M, "child", "map")),
      txn(0, "setup", set([M, "child"], "s", 0)),
      manage(0),
      txn(0, null, set([M, "child"], "a", 1)),
      undo(0, 0),
      txn(0, null, mdel(M, "child")),
      redo(0, 0),
    ],
  },
  {
    name: "undo-scope-recheck-two-managers",
    description: "a child-scoped manager does not delete what another manager restored into a new copy of the child",
    managers: [
      { doc: 0, scope: [M, "child"], origins: ["a"] },
      { doc: 0, scope: M, origins: ["b"] },
    ],
    states: true,
    steps: [
      txn(0, "setup", setType(M, "child", "map")),
      manage(0),
      manage(1),
      txn(0, "a", set([M, "child"], "x", 1)),
      txn(0, "b", mdel(M, "child")),
      undo(0, 1),
      undo(0, 0),
    ],
  },
  {
    name: "undo-into-deleted-restored-parent",
    description: "undo restores a key into a parent whose restored copy was deleted again: the step counts, the earlier one stays",
    managers: [
      { doc: 0, scope: M, origins: ["a"] },
      { doc: 0, scope: M, origins: ["b"] },
    ],
    states: true,
    steps: [
      txn(0, "setup", setType(M, "child", "map")),
      txn(0, "setup", set([M, "child"], "x", 1)),
      manage(0),
      manage(1),
      txn(0, "a", set(M, "z", 1)),
      txn(0, "a", mdel([M, "child"], "x")),
      txn(0, "b", mdel(M, "child")),
      undo(0, 1),
      txn(0, "other", mdel(M, "child")),
      undo(0, 0),
    ],
  },
  {
    name: "undo-group-restore-before-delete",
    description: "one undo step inserted a map and deleted a key an untracked edit put in it: yjs restores the key, then deletes the map",
    managers: [{ doc: 0, scope: M, group: true }],
    states: true,
    steps: [
      manage(0),
      txn(0, null, setType(M, "p", "map")),
      txn(0, "other", set([M, "p"], "c", 1)),
      txn(0, null, mdel([M, "p"], "c")),
      undo(0, 0),
    ],
  },
  {
    name: "undo-clear-releases-keep",
    description: "after clear, a map the manager kept is released: an untracked delete of it is collected",
    managers: [{ doc: 0, scope: M }],
    states: true,
    steps: [
      txn(0, "setup", setType(M, "child", "map")),
      txn(0, "setup", set([M, "child"], "a", 1)),
      manage(0),
      txn(0, null, mdel([M, "child"], "a")),
      clear(0, 0),
      txn(0, "other", mdel(M, "child")),
    ],
    diffs: true,
  },
  {
    name: "undo-fresh-edit-releases-keep",
    description: "undoing an insertion keeps what it deleted for redo; a fresh edit drops the redo step and releases the keep, so an untracked delete of the parent is collected",
    managers: [{ doc: 0, scope: M }],
    states: true,
    steps: [
      txn(0, "setup", setType(M, "child", "map")),
      manage(0),
      txn(0, null, set([M, "child"], "b", 2)),
      undo(0, 0),
      txn(0, null, set(M, "z", 1)),
      txn(0, "other", mdel(M, "child")),
    ],
    diffs: true,
  },
  {
    name: "undo-restore-order-concurrent-write",
    description: "one step deletes a key written by a middle client and the map holding it, with a lower client's write of the same key in between: yjs restores the lower client's value, because it walks deleting clients in deletion order",
    docs: 3,
    managers: [{ doc: 0, scope: M, group: true }],
    states: true,
    steps: [
      txn(0, "setup", setType(M, "p", "map")),
      apply(0, 1, "remote", "state"),
      apply(0, 2, "remote", "state"),
      txn(1, null, set([M, "p"], "x", "A")),
      apply(1, 0, "remote", "diff"),
      apply(1, 2, "remote", "diff"),
      manage(0),
      txn(2, null, set([M, "p"], "x", "B")),
      txn(0, null, mdel([M, "p"], "x")),
      apply(2, 0, "remote", "diff"),
      txn(0, null, mdel(M, "p")),
      undo(0, 0),
    ],
  },
  {
    name: "undo-release-before-keep",
    description: "a fresh edit drops a redo step that kept the map it then deletes: the release comes first, so the map stays kept and undo restores it with its key",
    managers: [{ doc: 0, scope: M }],
    states: true,
    steps: [
      txn(0, "setup", setType(M, "p", "map")),
      txn(0, "setup", set([M, "p"], "x", 1)),
      manage(0),
      txn(0, null, set([M, "p"], "y", 2)),
      undo(0, 0),
      txn(0, null, mdel(M, "p")),
      undo(0, 0),
    ],
    diffs: true,
  },
  {
    name: "undo-ends-capture-group",
    description: "an edit right after an undo opens a new step: the next undo removes only it",
    managers: [{ doc: 0, scope: T, group: true }],
    states: true,
    steps: [
      manage(0),
      txn(0, null, ins(T, 0, "a")),
      stop(0, 0),
      txn(0, null, ins(T, 1, "b")),
      undo(0, 0),
      txn(0, null, ins(T, 1, "c")),
      undo(0, 0),
    ],
  },
  {
    name: "undo-nested-multi-client",
    description: "a map created by a higher client ID holding keys from a lower one, deleted and undone by the lower: the parent is restored first",
    docs: 2,
    managers: [{ doc: 1, scope: M }],
    states: true,
    steps: [
      txn(0, null, setType(M, "p", "map")),
      apply(0, 1, "remote", "state"),
      manage(0),
      txn(1, "setup", set([M, "p"], "a", 1), set([M, "p"], "b", 2)),
      txn(1, null, mdel(M, "p")),
      undo(1, 0),
      redo(1, 0),
      undo(1, 0),
      apply(1, 0, "remote", "diff"),
    ],
  },
  {
    name: "undo-xml-element-delete",
    description: "an XML element with an attribute and a text child deleted and undone",
    managers: [{ doc: 0, scope: X }],
    states: true,
    steps: [
      txn(0, "setup", xmlElement(X, 0, "p")),
      txn(0, "setup", setAttr([X, 0], "class", "c"), xmlText([X, 0], 0)),
      txn(0, "setup", ins([X, 0, 0], 0, "hi")),
      manage(0),
      txn(0, null, del(X, 0, 1)),
      undo(0, 0),
      redo(0, 0),
      undo(0, 0),
    ],
  },
  {
    name: "undo-clear-then-tracked-delete",
    description: "clear releases what a dropped step kept, including the parent, before the next step keeps it again: the parent's deletion after clear is undone with its remaining key",
    managers: [{ doc: 0, scope: M }],
    states: true,
    steps: [
      txn(0, "setup", setType(M, "p", "map")),
      txn(0, "setup", set([M, "p"], "x", 1), set([M, "p"], "y", 2)),
      manage(0),
      txn(0, null, mdel([M, "p"], "x")),
      clear(0, 0),
      txn(0, null, mdel(M, "p")),
      undo(0, 0),
    ],
    diffs: true,
  },
  {
    name: "undo-clear-other-manager-keeps",
    description: "one manager's clear does not take away a keep another manager adds after it: the second manager still restores the shared parent",
    managers: [
      { doc: 0, scope: M, origins: ["a"] },
      { doc: 0, scope: M, origins: ["b"] },
    ],
    states: true,
    steps: [
      txn(0, "setup", setType(M, "p", "map")),
      txn(0, "setup", set([M, "p"], "x", 1), set([M, "p"], "y", 2)),
      manage(0),
      manage(1),
      txn(0, "a", mdel([M, "p"], "x")),
      clear(0, 0),
      txn(0, "b", mdel(M, "p")),
      undo(0, 1),
    ],
    diffs: true,
  },
  {
    name: "undo-clear-then-other-manager-undo",
    description: "one manager's clear, then another manager's undo restoring into the parent both kept: yjs releases at clear, before the undo keeps the parent again, so an untracked delete of the parent leaves its content",
    managers: [
      { doc: 0, scope: M, origins: ["a"] },
      { doc: 0, scope: M, origins: ["b"] },
    ],
    states: true,
    steps: [
      txn(0, "setup", setType(M, "p", "map")),
      txn(0, "setup", set([M, "p"], "x", 1), set([M, "p"], "y", 2)),
      manage(0),
      manage(1),
      txn(0, "a", mdel([M, "p"], "x")),
      txn(0, "b", mdel([M, "p"], "y")),
      clear(0, 0),
      undo(0, 1),
      txn(0, "other", mdel(M, "p")),
      redo(0, 1),
    ],
    diffs: true,
  },
  {
    name: "undo-tracked-remote-delete-order",
    description: "a tracked remote update deletes keys of two clients: yjs applies its delete set in the order written, highest client first, so undo walks the clients in that order and leaves a concurrent write in place",
    docs: 3,
    managers: [{ doc: 0, scope: M, group: true, origins: [null, "tracked"] }],
    states: true,
    steps: [
      txn(0, "setup", setType(M, "p", "map")),
      txn(2, "setup", set(M, "q", 1)),
      apply(0, 1, "remote", "state"),
      apply(2, 1, "remote", "state"),
      txn(1, "setup", set([M, "p"], "x", "A")),
      apply(1, 0, "remote", "state"),
      apply(1, 2, "remote", "state"),
      manage(0),
      txn(2, null, set([M, "p"], "x", "B")),
      txn(1, null, mdel([M, "p"], "x"), mdel(M, "q")),
      apply(1, 0, "tracked", "diff"),
      apply(2, 0, "remote", "diff"),
      txn(0, null, mdel(M, "p")),
      undo(0, 0),
    ],
  },
  {
    name: "undo-out-of-scope-deletion-first",
    description: "one tracked transaction deletes a key outside the manager's scope, then one inside: yjs keeps the whole transaction's client order, so undo walks the outside key's client first and restores the inside key over a concurrent write",
    docs: 3,
    managers: [{ doc: 0, scope: M, group: true }],
    states: true,
    steps: [
      txn(0, "setup", setType(M, "p", "map")),
      txn(2, "setup", set(O, "q", 1)),
      apply(0, 1, "remote", "state"),
      apply(2, 1, "remote", "state"),
      txn(1, "setup", set([M, "p"], "x", "A")),
      apply(1, 0, "remote", "state"),
      apply(1, 2, "remote", "state"),
      manage(0),
      txn(2, null, set([M, "p"], "x", "B")),
      txn(0, null, mdel(O, "q"), mdel([M, "p"], "x")),
      apply(2, 0, "remote", "diff"),
      txn(0, null, mdel(M, "p")),
      undo(0, 0),
    ],
  },
  {
    name: "undo-tracked-remote-blocks-before-deletes",
    description: "a tracked remote update whose higher client's write depends on a lower client's write in the same update: yjs integrates every block, both overwrites included, before the delete set, so the lower client's first deletion is an overwrite and undo walks that client before the delete set's others (GC off, so the overwritten write arrives with its content, not as a deleted struct)",
    docs: 3,
    gc: false,
    managers: [{ doc: 0, scope: M, origins: ["tracked"] }],
    states: true,
    steps: [
      txn(0, "setup", set(M, "k", "V0")),
      txn(2, "setup", set(M, "s", "old")),
      txn(1, "setup", set(M, "r", "w")),
      apply(0, 1, "remote", "state"),
      apply(2, 1, "remote", "state"),
      apply(1, 0, "remote", "state"),
      apply(1, 2, "remote", "state"),
      manage(0),
      txn(2, null, set(M, "k", "V1"), mdel(M, "s")),
      apply(2, 1, "remote", "diff"),
      txn(1, null, set(M, "k", "V2"), mdel(M, "r")),
      apply(1, 0, "tracked", "diff"),
      undo(0, 0),
    ],
  },
  {
    name: "undo-tracked-remote-insertions-order",
    description: "a tracked remote update inserts keys of two clients into an empty document: yjs records insertion clients in the order they joined the store and deletes in reverse on undo, which decides the order redo restores them in",
    docs: 4,
    managers: [{ doc: 0, scope: M, origins: ["tracked"] }],
    states: true,
    steps: [
      txn(1, null, set(M, "a", "A")),
      txn(2, null, set(M, "b", "B")),
      apply(1, 3, "remote", "state"),
      apply(2, 3, "remote", "state"),
      manage(0),
      apply(3, 0, "tracked", "state"),
      undo(0, 0),
      redo(0, 0),
    ],
  },
  {
    name: "undo-insertions-order-across-scope",
    description: "a tracked remote update inserts outside the manager's scope for a higher client and inside it for a lower one, then a grouped update inserts inside it for the higher: yjs keeps the insertion clients of the whole transaction in store order, so undo deletes the lower client's key last and redo restores it first",
    docs: 4,
    managers: [{ doc: 0, scope: M, group: true, origins: ["tracked"] }],
    states: true,
    steps: [
      manage(0),
      txn(1, null, set(O, "x", 0)),
      txn(2, null, set(M, "b", "B")),
      apply(1, 3, "remote", "state"),
      apply(2, 3, "remote", "state"),
      apply(3, 0, "tracked", "state"),
      txn(1, null, set(M, "a", "A")),
      apply(1, 0, "tracked", "diff"),
      undo(0, 0),
      redo(0, 0),
    ],
  },
  {
    name: "undo-redone-copy-collected",
    description: "a deletion whose restored copy was later collected: yjs still counts undoing it as a change, so one undo does not go on to undo the step before",
    managers: [{ doc: 0, scope: M, origins: ["a"] }, { doc: 0, scope: M, origins: ["a", "b"] }],
    states: true,
    steps: [
      txn(0, "setup", setType(M, "p", "map")),
      txn(0, "setup", set([M, "p"], "y", 1), set([M, "p"], "z", 2)),
      manage(0),
      manage(1),
      txn(0, "a", set(M, "w", 1)),
      txn(0, "a", mdel([M, "p"], "y")),
      txn(0, "b", mdel(M, "p")),
      undo(0, 1),
      undo(0, 1),
      txn(0, "b", mdel([M, "p"], "y")),
      clear(0, 1),
      txn(0, "other", mdel(M, "p")),
      undo(0, 0),
    ],
  },
  {
    name: "undo-fresh-update-before-pending",
    description: "an update arrives while a block of one of its clients waits in the pending queue for it: yjs integrates the update's own blocks first and retries the queue after, so a key the update sets comes before the queued one in _map, and undo, walking keys in that order, leaves a concurrent write in place",
    docs: 4,
    managers: [{ doc: 0, scope: M, group: true }],
    states: true,
    steps: [
      txn(0, "setup", setType(M, "p", "map")),
      apply(0, 1, "remote", "state"),
      apply(0, 2, "remote", "state"),
      apply(0, 3, "remote", "state"),
      txn(1, null, set(O, "dummy", 0)),
      apply(1, 3, "remote", "state"),
      txn(1, null, set([M, "p"], "x", "A")),
      txn(2, null, set([M, "p"], "q", "Q")),
      apply(2, 3, "remote", "state"),
      apply(1, 0, "remote", "since", { [CLIENTS[0]]: 1, [CLIENTS[1]]: 1 }),
      apply(3, 0, "remote", "diff"),
      apply(1, 2, "remote", "state"),
      txn(2, null, set([M, "p"], "x", "B")),
      manage(0),
      txn(0, null, mclear([M, "p"])),
      apply(2, 0, "remote", "diff"),
      txn(0, null, mdel(M, "p")),
      undo(0, 0),
    ],
  },
  {
    name: "undo-nested-keys-in-arrival-order",
    description: "a map holding keys from two other clients, z arriving before a: deleting the map deletes them in the order they were first set, as yjs walks _map, so undo restores z first",
    docs: 3,
    managers: [{ doc: 0, scope: M }],
    states: true,
    steps: [
      txn(0, "setup", setType(M, "p", "map")),
      apply(0, 1, "remote", "state"),
      apply(0, 2, "remote", "state"),
      txn(1, "setup", set([M, "p"], "z", 1)),
      txn(2, "setup", set([M, "p"], "a", 2)),
      apply(1, 0, "remote", "diff"),
      apply(2, 0, "remote", "diff"),
      manage(0),
      txn(0, null, mdel(M, "p")),
      undo(0, 0),
    ],
  },
  {
    name: "undo-nested-keys-from-one-update",
    description: "a map whose keys from two clients arrive in one update: yjs integrates the higher client first, so its key comes first in _map and is restored first",
    docs: 3,
    managers: [{ doc: 0, scope: M }],
    states: true,
    steps: [
      txn(0, "setup", setType(M, "p", "map")),
      apply(0, 1, "remote", "state"),
      apply(0, 2, "remote", "state"),
      txn(1, "setup", set([M, "p"], "z", 1)),
      txn(2, "setup", set([M, "p"], "a", 2)),
      apply(2, 1, "remote", "diff"),
      apply(1, 0, "remote", "diff"),
      manage(0),
      txn(0, null, mdel(M, "p")),
      undo(0, 0),
    ],
  },
  {
    name: "undo-keys-from-dependent-blocks",
    description: "one update holds keys of three clients in a map: the highest client's key overwrites the lowest's, and a middle client's key is independent. yjs integrates the highest client's key first, following its dependency into just the lowest client's first key, then the middle client's, then the rest, so the keys first set come in that order and undo of the map's deletion restores in it",
    docs: 4,
    managers: [{ doc: 0, scope: M }],
    states: true,
    steps: [
      txn(0, "setup", setType(M, "p", "map")),
      apply(0, 1, "remote", "state"),
      apply(0, 2, "remote", "state"),
      apply(0, 3, "remote", "state"),
      txn(3, null, set([M, "p"], "z", "L1")),
      apply(3, 1, "remote", "diff"),
      txn(1, null, set([M, "p"], "z", "H1")),
      txn(3, null, set([M, "p"], "a", "L2")),
      txn(2, null, set([M, "p"], "m", "M1")),
      apply(3, 1, "remote", "diff"),
      apply(2, 1, "remote", "diff"),
      manage(0),
      apply(1, 0, "remote", "diff"),
      txn(0, null, mdel(M, "p")),
      undo(0, 0),
    ],
  },
  {
    name: "undo-map-clear-order",
    description: "a map with keys from two clients, z set before a, cleared in a tracked transaction: yjs clears in _map order, and undo restores in that order",
    docs: 2,
    managers: [{ doc: 0, scope: M }],
    states: true,
    steps: [
      txn(1, "setup", set(M, "z", 1)),
      apply(1, 0, "remote", "state"),
      txn(0, "setup", set(M, "a", 2)),
      manage(0),
      txn(0, null, mclear(M)),
      undo(0, 0),
    ],
  },
  {
    name: "undo-insertion-merged-into-restored-copy",
    description: "text typed right after a restored copy merges with it in yjs, kept or not, so undoing the original insertion deletes the typed text with it",
    managers: [{ doc: 0, scope: M }],
    states: true,
    steps: [
      txn(0, "setup", setType(M, "t", "text")),
      manage(0),
      txn(0, null, ins([M, "t"], 0, "c")),
      txn(0, null, mdel(M, "t")),
      undo(0, 0),
      txn(0, "other", ins([M, "t"], 1, "xyz")),
      undo(0, 0),
    ],
  },
  {
    name: "undo-deletes-merged-restored-copy",
    description: "a restore brings back an insertion and text typed after it as one struct in yjs; undoing the insertion through the other manager deletes the whole merged copy",
    managers: [{ doc: 0, scope: M }, { doc: 0, scope: M, origins: ["b"] }],
    states: true,
    steps: [
      txn(0, "setup", setType(M, "t", "text")),
      manage(0),
      manage(1),
      txn(0, "b", ins([M, "t"], 0, "c")),
      txn(0, "setup", set(M, "z", 1)),
      txn(0, "setup", ins([M, "t"], 1, "xyz")),
      txn(0, null, mdel(M, "t")),
      undo(0, 0),
      undo(0, 1),
    ],
  },
  {
    name: "undo-array-restored-copy-merge",
    description: "an array element restored by undo merges with an element pushed right after it, as yjs merges kept items, so undoing the push deletes both",
    managers: [{ doc: 0, scope: A }],
    states: true,
    steps: [
      txn(0, "setup", push(A, ["a", "b"])),
      manage(0),
      txn(0, null, push(A, ["c"])),
      txn(0, null, del(A, 2, 1)),
      undo(0, 0),
      txn(0, "other", insv(A, 3, ["x"])),
      undo(0, 0),
    ],
  },
  {
    name: "array-insert-after-undo",
    description: "an undo deletes an array element before the one a search marker sits on; the next insert by index lands where yjs puts it (yjs popStackItem resets the markers)",
    managers: [{ doc: 0, scope: A }],
    states: true,
    steps: [
      manage(0),
      txn(0, null, insType(A, 0, "array")),
      txn(0, "y", insv(A, 1, [2])),
      undo(0, 0),
      txn(0, "other", insType(A, 1, "array")),
    ],
  },
  {
    name: "array-insert-after-remote-delete",
    description: "a remote delete removes an array element before the one a search marker sits on; the next local insert by index lands where yjs puts it (yjs resets the markers of a remote transaction)",
    docs: 2,
    states: true,
    steps: [
      txn(0, null, insType(A, 0, "array")),
      txn(0, null, insv(A, 1, [2])),
      apply(0, 1, "remote", "state"),
      txn(1, null, del(A, 0, 1)),
      apply(1, 0, "remote", "diff"),
      txn(0, null, insType(A, 1, "array")),
    ],
  },
  {
    name: "undo-redo",
    description: "UndoManager transactions emit updates with the manager as origin",
    steps: [txn(0, null, ins(T, 0, "abc")), txn(0, null, ins(T, 3, "def")), undo(0), undo(0), redo(0)],
    undoScope: T,
  },
];

const resolveType = (doc, path) => {
  const segs = Array.isArray(path) ? path : [path];
  const [kind, name] = segs[0].split(":");
  let cur;
  switch (kind) {
    case "text": cur = doc.getText(name); break;
    case "array": cur = doc.getArray(name); break;
    case "map": cur = doc.getMap(name); break;
    case "xml": cur = doc.getXmlFragment(name); break;
    default: throw new Error(`unknown root kind ${kind}`);
  }
  for (const seg of segs.slice(1)) {
    cur = cur.get(seg);
    if (cur === undefined) throw new Error(`no type at ${JSON.stringify(path)}`);
  }
  return cur;
};

const newType = (kind) => {
  switch (kind) {
    case "map": return new Y.Map();
    case "array": return new Y.Array();
    case "text": return new Y.Text();
    default: throw new Error(`unknown type kind ${kind}`);
  }
};

// yjs writes null into the attributes object it is given, so every call gets
// a deep copy and the recorded op keeps the caller's values.
const copy = (v) => (v === undefined ? undefined : structuredClone(v));

const applyOp = (doc, op) => {
  const t = resolveType(doc, op.path);
  switch (op.op) {
    case "insert": return op.attrs ? t.insert(op.index, op.text, copy(op.attrs)) : t.insert(op.index, op.text);
    case "delete": return t.delete(op.index, op.len);
    case "format": return t.format(op.index, op.len, copy(op.attrs));
    case "embed": return t.insertEmbed(op.index, copy(op.value));
    case "push": return t.push(copy(op.values));
    case "insertValues": return t.insert(op.index, copy(op.values));
    case "set": return t.set(op.key, copy(op.value));
    case "setType": return t.set(op.key, newType(op.kind));
    case "insertType": return t.insert(op.index, [newType(op.kind)]);
    case "mapDelete": return t.delete(op.key);
    case "mapClear": return t.clear();
    case "xmlElement": return t.insert(op.index, [new Y.XmlElement(op.name)]);
    case "xmlText": return t.insert(op.index, [new Y.XmlText()]);
    case "setAttr": return t.setAttribute(op.key, op.value);
    default: throw new Error(`unknown op ${op.op}`);
  }
};

const hex = (u8) => Buffer.from(u8).toString("hex");
const svBytes = (sv) => Y.encodeStateVector(new Map(Object.entries(sv).map(([c, k]) => [Number(c), k])));

const out = scenarios.map((sc) => {
  const n = sc.docs || 1;
  const gc = sc.gc !== false;
  const docs = [];
  for (let i = 0; i < n; i++) {
    const d = new Y.Doc({ gc });
    d.clientID = CLIENTS[i];
    docs.push(d);
  }
  // undoGroup: one undo step for everything (a capture timeout no test
  // reaches); otherwise every transaction is its own step. The manager
  // tracks the null origin only, so a "setup" transaction is not undoable.
  const ums = docs.map((d) => (sc.undoScope ? new Y.UndoManager(resolveType(d, sc.undoScope), { captureTimeout: sc.undoGroup ? 1e9 : 0 }) : null));
  const managers = (sc.managers || []).map(() => null);
  const events = [];
  let step = -1;
  const originName = (o, i) => {
    if (o === null || o === undefined) return null;
    if (typeof o === "string") return o;
    if (o === ums[i]) return "<undo-manager>";
    const k = managers.indexOf(o);
    if (k >= 0) return `<um:${k}>`;
    throw new Error(`unexpected origin ${o}`);
  };
  const managerAt = (u) => (u.um === undefined ? ums[u.doc] : managers[u.um]);
  const rootKeys = [...new Set(sc.steps.flatMap((s) => (s.ops || []).map((op) => (Array.isArray(op.path) ? op.path[0] : op.path))).concat(sc.undoScope ? [sc.undoScope] : []).concat((sc.managers || []).map((m) => (Array.isArray(m.scope) ? m.scope[0] : m.scope))))].sort();
  const snapshot = () => ({
    state: docs.map((d) => Object.fromEntries(rootKeys.map((r) => [r, resolveType(d, r).toJSON()]))),
    stacks: managers.map((m) => (m ? [m.canUndo(), m.canRedo()] : null)),
  });
  docs.forEach((d, i) => {
    d.on("update", (u, origin) => events.push({ step, doc: i, origin: originName(origin, i), v1: hex(u) }));
    d.on("updateV2", (u, origin) => {
      const ev = events.findLast((e) => e.step === step && e.doc === i && e.v2 === undefined);
      if (!ev) throw new Error("updateV2 without a matching update");
      if (ev.origin !== originName(origin, i)) throw new Error("updateV2 origin differs from update");
      ev.v2 = hex(u);
    });
  });
  const steps = [];
  sc.steps.forEach((s, k) => {
    step = k;
    if (s.apply) {
      const { from, to, origin, what, sv } = s.apply;
      let u;
      if (s.apply.replay !== undefined) u = Buffer.from(steps[s.apply.replay].apply.update, "hex");
      else if (what === "state") u = Y.encodeStateAsUpdate(docs[from]);
      else if (what === "diff") u = Y.encodeStateAsUpdate(docs[from], Y.encodeStateVector(docs[to]));
      else if (what === "since") u = Y.encodeStateAsUpdate(docs[from], svBytes(sv));
      else throw new Error(`unknown apply ${what}`);
      Y.applyUpdate(docs[to], u, origin);
      steps.push({ apply: { ...s.apply, update: hex(u) } });
    } else if (s.undo || s.redo) {
      const um = managerAt(s.undo || s.redo);
      const result = (s.undo ? um.undo() : um.redo()) !== null;
      steps.push(sc.managers ? { ...s, result } : s);
    } else if (s.manage !== undefined) {
      const m = sc.managers[s.manage];
      managers[s.manage] = new Y.UndoManager(resolveType(docs[m.doc], m.scope), {
        captureTimeout: m.group ? 1e9 : 0,
        trackedOrigins: new Set(m.origins === undefined ? [null] : m.origins),
      });
      steps.push(s);
    } else if (s.clear) {
      managers[s.clear.um].clear();
      steps.push(s);
    } else if (s.stop) {
      managers[s.stop.um].stopCapturing();
      steps.push(s);
    } else {
      docs[s.doc].transact(() => s.ops.forEach((op) => applyOp(docs[s.doc], op)), s.origin);
      steps.push(s);
    }
    if (sc.states) Object.assign(steps[steps.length - 1], snapshot());
  });
  for (const e of events) {
    if (e.v2 === undefined) throw new Error(`${sc.name}: update without updateV2`);
  }
  let diffs;
  if (sc.diffs) {
    diffs = [];
    const doc = docs[sc.diffsDoc || 0];
    const state = Y.decodeStateVector(Y.encodeStateVector(doc));
    const clients = [...state.keys()].sort((a, b) => b - a);
    // Grid over every client's clocks 0..state (inclusive). Two clients
    // with short histories keep this small.
    const grid = clients.reduce((acc, c) => acc.flatMap((sv) => {
      const rows = [];
      for (let k = 0; k <= state.get(c); k++) rows.push({ ...sv, [c]: k });
      return rows;
    }), [{}]);
    if (grid.length > 400) throw new Error(`${sc.name}: diff grid of ${grid.length} is too large`);
    for (const sv of grid) {
      const enc = svBytes(sv);
      diffs.push({ sv, v1: hex(Y.encodeStateAsUpdate(doc, enc)), v2: hex(Y.encodeStateAsUpdateV2(doc, enc)) });
    }
  }
  return {
    name: sc.name,
    description: sc.description,
    gc,
    clients: CLIENTS.slice(0, n),
    ...(sc.undoScope ? { undo_scope: sc.undoScope, undo_group: !!sc.undoGroup } : {}),
    ...(sc.managers ? { managers: sc.managers } : {}),
    ...(sc.states ? { root_keys: [...new Set(sc.steps.flatMap((s) => (s.ops || []).map((op) => (Array.isArray(op.path) ? op.path[0] : op.path))).concat((sc.managers || []).map((m) => (Array.isArray(m.scope) ? m.scope[0] : m.scope))))].sort() } : {}),
    steps,
    events,
    ...(diffs ? { diffs_doc: sc.diffsDoc || 0, diffs } : {}),
  };
});

const names = new Set();
for (const sc of out) {
  if (names.has(sc.name)) throw new Error(`duplicate scenario name ${sc.name}`);
  names.add(sc.name);
}

writeFileSync(
  outPath,
  JSON.stringify({ generator: "gen-update-events.mjs (yjs@13.6.33)", scenarios: out }, null, 2) + "\n",
);
const nEvents = out.reduce((s, sc) => s + sc.events.length, 0);
const nDiffs = out.reduce((s, sc) => s + (sc.diffs ? sc.diffs.length : 0), 0);
console.log(`wrote ${out.length} scenarios (${nEvents} events, ${nDiffs} diffs) to ${outPath}`);
