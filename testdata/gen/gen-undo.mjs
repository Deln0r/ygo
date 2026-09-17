// Generates testdata/undo-fixtures.json — cross-language UndoManager
// conformance fixtures captured from JS Yjs (yjs@13.6.32).
//
// The UndoManager is a local-only concept: there is no wire format for
// the undo / redo stacks. So the cross-language check is semantic, not
// byte-level. Each scenario runs an identical operation + undo/redo
// sequence in yjs and records the FINAL document state. The Go test in
// undo_fixtures_test.go runs the same sequence via ygo's UndoManager
// and asserts it lands on the same state.
//
// Scenario logic is duplicated (here in JS, there in Go) on purpose:
// there is no shared op-encoding, so what crosses the language boundary
// is the reference end-state that yjs arrived at.
//
// Run: node gen-undo.mjs (after npm install in this directory).

import { writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";
import * as Y from "yjs";

const here = dirname(fileURLToPath(import.meta.url));
const outPath = resolve(here, "..", "undo-fixtures.json");

// Each scenario returns the final state as a JSON-serializable value.
// `kind` tells the Go side how to read the state back (map/array/text).
const scenarios = [];

function mapState(map) {
  const out = {};
  for (const k of map.keys()) {
    const v = map.get(k);
    if (v !== undefined) out[k] = v;
  }
  return out;
}

// 1. Map set, then undo: key gone.
scenarios.push({
  description: "map set then undo",
  kind: "map",
  root: "m",
  run() {
    const doc = new Y.Doc();
    const m = doc.getMap("m");
    const um = new Y.UndoManager(m, { captureTimeout: 0 });
    m.set("theme", "dark");
    um.undo();
    return mapState(m);
  },
});

// 2. Map set, undo, redo: key back.
scenarios.push({
  description: "map set then undo then redo",
  kind: "map",
  root: "m",
  run() {
    const doc = new Y.Doc();
    const m = doc.getMap("m");
    const um = new Y.UndoManager(m, { captureTimeout: 0 });
    m.set("theme", "dark");
    um.undo();
    um.redo();
    return mapState(m);
  },
});

// 3. Map overwrite, undo: reverts to first value.
scenarios.push({
  description: "map overwrite then undo reverts to first",
  kind: "map",
  root: "m",
  run() {
    const doc = new Y.Doc();
    const m = doc.getMap("m");
    const um = new Y.UndoManager(m, { captureTimeout: 0 });
    m.set("k", "first");
    m.set("k", "second");
    um.undo();
    return mapState(m);
  },
});

// 4. Array insert, undo, redo: element back.
scenarios.push({
  description: "array insert then undo then redo",
  kind: "array",
  root: "a",
  run() {
    const doc = new Y.Doc();
    const a = doc.getArray("a");
    const um = new Y.UndoManager(a, { captureTimeout: 0 });
    a.insert(0, ["x"]);
    um.undo();
    um.redo();
    return a.toArray();
  },
});

// 5. Array delete, undo: element restored at position.
scenarios.push({
  description: "array delete then undo restores",
  kind: "array",
  root: "a",
  run() {
    const doc = new Y.Doc();
    const a = doc.getArray("a");
    const um = new Y.UndoManager(a, { captureTimeout: 0 });
    a.insert(0, ["a", "b"]);
    um.stopCapturing();
    a.delete(0, 1);
    um.undo();
    return a.toArray();
  },
});

// 6. Text insert, undo: empty.
scenarios.push({
  description: "text insert then undo",
  kind: "text",
  root: "t",
  run() {
    const doc = new Y.Doc();
    const t = doc.getText("t");
    const um = new Y.UndoManager(t, { captureTimeout: 0 });
    t.insert(0, "hello");
    um.undo();
    return t.toString();
  },
});

// 7. Text delete, undo: restored.
scenarios.push({
  description: "text delete then undo restores",
  kind: "text",
  root: "t",
  run() {
    const doc = new Y.Doc();
    const t = doc.getText("t");
    const um = new Y.UndoManager(t, { captureTimeout: 0 });
    t.insert(0, "hello world");
    um.stopCapturing();
    t.delete(5, 6);
    um.undo();
    return t.toString();
  },
});

// Text with its formatting and length, rendered identically on the Go side:
// each delta op is its text followed by its attributes as JSON with sorted
// keys, ops joined by "|", then " #" and the length. Length is in the state
// because a restored format marker that counts as content shows up nowhere
// else.
function textState(t) {
  const ops = t.toDelta().map((op) => {
    if (typeof op.insert !== "string") throw new Error("textState renders text only");
    if (!op.attributes) return op.insert;
    const keys = Object.keys(op.attributes).sort();
    return op.insert + JSON.stringify(Object.fromEntries(keys.map((k) => [k, op.attributes[k]])));
  });
  return `${ops.join("|")} #${t.length}`;
}

// 8-10. An insert and a delete of part of it inside one captured step. yjs
// never resurrects what the same step created, so undo leaves nothing behind.
scenarios.push({
  description: "text insert and delete in one transaction then undo",
  kind: "text",
  root: "t",
  run() {
    const doc = new Y.Doc();
    const t = doc.getText("t");
    const um = new Y.UndoManager(t, { captureTimeout: 0 });
    doc.transact(() => {
      t.insert(0, "abc");
      t.delete(1, 1);
    });
    um.undo();
    return t.toString();
  },
});

scenarios.push({
  description: "text insert and delete in one transaction then undo then redo",
  kind: "text",
  root: "t",
  run() {
    const doc = new Y.Doc();
    const t = doc.getText("t");
    const um = new Y.UndoManager(t, { captureTimeout: 0 });
    doc.transact(() => {
      t.insert(0, "abc");
      t.delete(1, 1);
    });
    um.undo();
    um.redo();
    return t.toString();
  },
});

scenarios.push({
  description: "text typed then corrected within one capture window then undo",
  kind: "text",
  root: "t",
  run() {
    const doc = new Y.Doc();
    const t = doc.getText("t");
    const um = new Y.UndoManager(t, { captureTimeout: 1e9 });
    t.insert(0, "abc");
    t.delete(1, 1);
    um.undo();
    return t.toString();
  },
});

// 11. Undo and redo of formatted text restore its markers without counting
// them as content.
scenarios.push({
  description: "formatted insert then undo then redo",
  kind: "text-formatted",
  root: "t",
  run() {
    const doc = new Y.Doc();
    const t = doc.getText("t");
    const um = new Y.UndoManager(t, { captureTimeout: 0 });
    t.insert(0, "abc", { bold: true });
    um.undo();
    um.redo();
    return textState(t);
  },
});

// 12-13. Formatting over a run of the same attribute deletes that run's
// markers; undo has to bring them back and redo has to take them away again.
scenarios.push({
  description: "format over a run then undo",
  kind: "text-formatted",
  root: "t",
  run() {
    const doc = new Y.Doc();
    const t = doc.getText("t");
    const um = new Y.UndoManager(t, { captureTimeout: 0 });
    t.insert(0, "abcdef");
    t.format(2, 2, { bold: true });
    t.format(0, 6, { bold: false });
    um.undo();
    return textState(t);
  },
});

scenarios.push({
  description: "format over a run then undo then redo",
  kind: "text-formatted",
  root: "t",
  run() {
    const doc = new Y.Doc();
    const t = doc.getText("t");
    const um = new Y.UndoManager(t, { captureTimeout: 0 });
    t.insert(0, "abcdef");
    t.format(2, 2, { bold: true });
    t.format(0, 6, { bold: false });
    um.undo();
    um.redo();
    return textState(t);
  },
});

// 14-15. Clearing an attribute on part of a run, then undo, then redo.
scenarios.push({
  description: "clear part of a formatted run then undo",
  kind: "text-formatted",
  root: "t",
  run() {
    const doc = new Y.Doc();
    const t = doc.getText("t");
    const um = new Y.UndoManager(t, { captureTimeout: 0 });
    t.insert(0, "bold text", { bold: true });
    t.format(0, 4, { bold: null });
    um.undo();
    return textState(t);
  },
});

scenarios.push({
  description: "clear part of a formatted run then undo then redo",
  kind: "text-formatted",
  root: "t",
  run() {
    const doc = new Y.Doc();
    const t = doc.getText("t");
    const um = new Y.UndoManager(t, { captureTimeout: 0 });
    t.insert(0, "bold text", { bold: true });
    t.format(0, 4, { bold: null });
    um.undo();
    um.redo();
    return textState(t);
  },
});

// 16. Two neighbouring characters deleted in separate steps; undoing the
// second step restores only the second character.
scenarios.push({
  description: "delete neighbours in separate steps then undo once",
  kind: "text",
  root: "t",
  run() {
    const doc = new Y.Doc();
    const t = doc.getText("t");
    const um = new Y.UndoManager(t, { captureTimeout: 0 });
    t.insert(0, "abcd");
    t.delete(1, 1);
    t.delete(1, 1);
    um.undo();
    return t.toString();
  },
});

const out = {
  generator: "yjs@13.6.32 (UndoManager)",
  scenarios: scenarios.map((s) => ({
    description: s.description,
    kind: s.kind,
    root: s.root,
    expected: s.run(),
  })),
};

writeFileSync(outPath, JSON.stringify(out, null, 2) + "\n");
console.log(`wrote ${out.scenarios.length} undo scenarios to ${outPath}`);
