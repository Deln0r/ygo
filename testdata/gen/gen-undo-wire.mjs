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
// {t: "redo"} drive the UndoManager. Every scenario runs with garbage
// collection on and off (gc), and the Go test delivers the updates as
// written (V1) and as yjs converts them (updatesV2).
//
// "crafted" scenarios are updates written by hand, as another writer could
// send them: blocks that hang off a garbage-collected run, a known block
// whose right origin alone points inside an item an undo brought back, a
// block the receiver knows in part.
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
  for (const gc of [true, false]) scenarioGC(`${name} (gc ${gc ? "on" : "off"})`, kind, gc, updates, steps);
}
function scenarioGC(name, kind, gc, updates, steps) {
  const d = new Y.Doc({ gc });
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
  scenarios.push({
    name, kind, gc, steps, states,
    updates: updates.map(toHex),
    updatesV2: updates.map((u) => toHex(Y.convertUpdateFormatV1ToV2(u))),
  });
}
const fromHex = (h) => Uint8Array.from(h.replace(/ /g, "").match(/../g).map((x) => parseInt(x, 16)));

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

// Crafted updates on an array "r" holding w, x, y, z from client 1
// (1:0+4), which the receiver deletes and brings back with an undo, so
// that item is marked as redone and its pieces do not merge again.
{
  const src = new Y.Doc();
  src.clientID = 1;
  src.getArray("r").insert(0, ["w", "x", "y", "z"]);
  const base = Y.encodeStateAsUpdate(src);
  const gcRun = "0102020000" + "01"; // client 2: a GC run 2:0+1, then one more block
  const pre = [{ t: "up", i: 0 }, { t: "del", i: 0, n: 4 }, { t: "undo" }];
  const crafted = (name, updates, steps) => scenario(`crafted: ${name}`, "array", [base, ...updates.map(fromHex)], steps);
  // A new block 2:1 after the GC run: origin in the run, right origin
  // inside the redone item; it integrates as GC, and still cuts.
  crafted("a new block with its origin in a GC run and its right origin inside a redone item",
    [gcRun + "c8" + "0200" + "0102" + "017d01" + "00"], [...pre, { t: "up", i: 1 }, { t: "up", i: 1 }]);
  crafted("a new block with its origin inside a redone item and its right origin in a GC run",
    [gcRun + "c8" + "0101" + "0200" + "017d01" + "00"], [...pre, { t: "up", i: 1 }, { t: "up", i: 1 }]);
  crafted("the GC run and the block after it in two updates",
    ["0101020000" + "01" + "00", "01010201" + "c8" + "0200" + "0102" + "017d01" + "00"], [...pre, { t: "up", i: 1 }, { t: "up", i: 2 }, { t: "up", i: 2 }]);
  // Client 2 first inserts r at 0 (2:0); then a block 2:0+2 the receiver
  // knows in part, origin and right origin inside the redone item.
  crafted("a block known in part, its origin and right origin inside a redone item",
    ["01010200" + "08" + "010172" + "017d01" + "00", "01010200" + "c8" + "0101" + "0103" + "027d017d02" + "00"],
    [{ t: "up", i: 0 }, { t: "up", i: 1 }, { t: "del", i: 1, n: 4 }, { t: "undo" }, { t: "up", i: 2 }, { t: "up", i: 2 }]);
  // A known block with only a right origin, inside the redone item.
  crafted("a known block whose right origin alone is inside a redone item",
    ["01010200" + "08" + "010172" + "017d01" + "00", "01010200" + "48" + "0102" + "017d01" + "00"],
    [{ t: "up", i: 0 }, { t: "up", i: 1 }, { t: "del", i: 1, n: 4 }, { t: "undo" }, { t: "up", i: 2 }]);
  // No undo: the cuts merge back.
  crafted("a known block cutting an item no undo brought back",
    ["01010100" + "c8" + "0101" + "0103" + "017d01" + "00"], [{ t: "up", i: 0 }, { t: "up", i: 1 }, { t: "del", i: 1, n: 1 }, { t: "up", i: 1 }]);
}

// Cuts that split a surrogate pair change the text for good: yjs makes
// both halves U+FFFD (ContentString.splice), and the pieces merge back
// like that. No undo is involved.
{
  const src = new Y.Doc();
  src.clientID = 1;
  src.getText("r").insert(0, "a\u{1F600}b");
  const base = Y.encodeStateAsUpdate(src);
  const text = (name, updates, steps) => scenario(`surrogates: ${name}`, "text", [base, ...updates.map(fromHex)], steps);
  text("a known block whose origin is the high surrogate", ["01010103" + "84" + "0101" + "0162" + "00"], [{ t: "up", i: 0 }, { t: "up", i: 1 }]);
  text("a known block whose right origin is the low surrogate", ["01010103" + "44" + "0102" + "0162" + "00"], [{ t: "up", i: 0 }, { t: "up", i: 1 }]);
  text("a block hanging off a GC run, its right origin the low surrogate",
    ["0102020000" + "01" + "c4" + "0200" + "0102" + "0178" + "00"], [{ t: "up", i: 0 }, { t: "up", i: 1 }]);
  // A peer types the pair in two inserts; the receiver takes its full
  // state, then the second update again.
  const p = new Y.Doc();
  p.clientID = 3;
  const ups = [];
  p.on("update", (u) => ups.push(u));
  p.getText("r").insert(0, "a\uD83D");
  p.getText("r").insert(2, "\uDE00b");
  scenario("surrogates: a pair typed in two inserts, the full state, then the second insert again", "text",
    [Y.encodeStateAsUpdate(p), ups[1]], [{ t: "up", i: 0 }, { t: "up", i: 1 }]);
}
// A block known in part whose right origin lies inside the item that
// holds its last known clock: the cuts at its references come before its
// left is taken from that clock.
{
  const base = fromHex("02010200880103027d047d05010100080103617272047d007d017d027d0300");
  scenario("crafted: a block known in part, its right origin inside the item holding its last known clock", "array",
    [base, fromHex("01010102480101037d0a7d0b7d0c00")], [{ t: "up", i: 0 }, { t: "up", i: 1 }]);
}

writeFileSync(outPath, JSON.stringify({ generator: "gen-undo-wire.mjs (yjs 13.6.33)", scenarios }, null, 2) + "\n");
console.log(`wrote ${scenarios.length} scenarios to ${outPath}`);
