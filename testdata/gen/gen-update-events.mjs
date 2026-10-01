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

// Client IDs. Large enough that their varints take several bytes, and doc 0
// sorts above doc 1, so the descending client order in an update is visible.
const CLIENTS = [3141592653, 271828182];

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

// Steps.
const txn = (doc, origin, ...ops) => ({ doc, origin, ops });
// what: "state" applies the full state of `from`; "diff" applies the diff
// against the current state vector of `to`; "since" applies the diff against
// an explicit state vector `sv` ({client: clock}).
const apply = (from, to, origin, what, sv) => ({ apply: { from, to, origin, what, ...(sv ? { sv } : {}) } });
// replay applies again the exact bytes an earlier apply step sent.
const replay = (step, to, origin) => ({ apply: { replay: step, to, origin } });
const undo = (doc) => ({ undo: { doc } });
const redo = (doc) => ({ redo: { doc } });

const T = "text:t";
const A = "array:a";
const M = "map:m";

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
  const events = [];
  let step = -1;
  const originName = (o, i) => {
    if (o === null || o === undefined) return null;
    if (typeof o === "string") return o;
    if (o === ums[i]) return "<undo-manager>";
    throw new Error(`unexpected origin ${o}`);
  };
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
      const i = (s.undo || s.redo).doc;
      if (s.undo) ums[i].undo();
      else ums[i].redo();
      steps.push(s);
    } else {
      docs[s.doc].transact(() => s.ops.forEach((op) => applyOp(docs[s.doc], op)), s.origin);
      steps.push(s);
    }
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
