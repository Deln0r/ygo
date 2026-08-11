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
