// Generates testdata/undo-wire-fixtures.json: a receiver with an
// UndoManager edits around updates from a peer, from yjs 13.6.33, and its
// state is recorded in V1 and V2 after every step. The UndoManager's marks
// on items (kept tombstones, items an undo brought back) are local, never
// on the wire, so the Go test (undo_wire_fixture_test.go) replays the
// receiver's own steps with ygo's UndoManager and requires yjs's bytes at
// each step.
//
// Steps: {t: "up", i} applies updates[i] with origin "remote"; {t: "del",
// i, n} and {t: "ins", i, v} edit the root locally; {t: "undo"} and
// {t: "redo"} drive the UndoManager.
//
// Run: node gen-undo-wire.mjs (after npm install in this directory).

import { writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";
import * as Y from "yjs";

const here = dirname(fileURLToPath(import.meta.url));
const outPath = resolve(here, "..", "undo-wire-fixtures.json");
const toHex = (u8) => Array.from(u8, (b) => b.toString(16).padStart(2, "0")).join("");

// updatesOf returns the updates a peer (client 3) sends, one per edit.
function updatesOf(kind, edits) {
  const d = new Y.Doc();
  d.clientID = 3;
  const root = kind === "text" ? d.getText("r") : d.getArray("r");
  const out = [];
  for (const e of edits) {
    const sv = Y.encodeStateVector(d);
    e(root);
    out.push(Y.encodeStateAsUpdate(d, sv));
  }
  return out;
}

const scenarios = [];
function scenario(name, kind, updates, steps) {
  const d = new Y.Doc();
  d.clientID = 99;
  const root = kind === "text" ? d.getText("r") : d.getArray("r");
  const um = new Y.UndoManager(root, { captureTimeout: 1e12 });
  const states = [];
  for (const s of steps) {
    switch (s.t) {
      case "up": Y.applyUpdate(d, updates[s.i], "remote"); break;
      case "del": root.delete(s.i, s.n); break;
      case "ins": kind === "text" ? root.insert(s.i, s.v) : root.insert(s.i, [s.v]); break;
      case "undo": um.undo(); break;
      case "redo": um.redo(); break;
      default: throw new Error(s.t);
    }
    states.push({ v1: toHex(Y.encodeStateAsUpdate(d)), v2: toHex(Y.encodeStateAsUpdateV2(d)) });
  }
  scenarios.push({ name, kind, updates: updates.map(toHex), steps, states });
}

// The peer appends two pieces; the receiver, which merged them into one
// item, deletes them, undoes the delete, and gets the second piece again.
// Resolving the repeated piece's origin cuts the merged item, and the
// pieces an undo brought back do not merge again.
const pieces = (kind) => updatesOf(kind, [
  (r) => (kind === "text" ? r.insert(0, "ab") : r.insert(0, ["a", "b"])),
  (r) => (kind === "text" ? r.insert(2, "cd") : r.insert(2, ["c", "d"])),
]);
for (const kind of ["array", "text"]) {
  scenario(`${kind}: an update delivered twice after a delete and its undo`, kind, pieces(kind), [
    { t: "up", i: 0 }, { t: "up", i: 1 }, { t: "del", i: 0, n: 4 }, { t: "undo" }, { t: "up", i: 1 },
  ]);
  scenario(`${kind}: an update delivered twice after a delete, its undo and redo`, kind, pieces(kind), [
    { t: "up", i: 0 }, { t: "up", i: 1 }, { t: "del", i: 0, n: 4 }, { t: "undo" }, { t: "redo" }, { t: "up", i: 1 },
  ]);
}
// The repeated update is the first piece, which the second depends on:
// its right origin is none, its origin none, so nothing is cut.
scenario("array: the first update delivered twice after a delete and its undo", "array", pieces("array"), [
  { t: "up", i: 0 }, { t: "up", i: 1 }, { t: "del", i: 0, n: 4 }, { t: "undo" }, { t: "up", i: 0 },
]);
// A local insert between the pieces, then a repeat of the second piece.
scenario("array: an update delivered twice after an insert, a delete and its undo", "array", pieces("array"), [
  { t: "up", i: 0 }, { t: "up", i: 1 }, { t: "ins", i: 4, v: "e" }, { t: "del", i: 1, n: 2 }, { t: "undo" }, { t: "up", i: 1 }, { t: "up", i: 0 },
]);

writeFileSync(outPath, JSON.stringify({ generator: "gen-undo-wire.mjs (yjs 13.6.33)", scenarios }, null, 2) + "\n");
console.log(`wrote ${scenarios.length} scenarios to ${outPath}`);
