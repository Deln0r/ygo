// Generates testdata/format-undefined-fixtures.json: what yjs 13.6.33 does
// with a format attribute whose value is undefined itself, and with an
// undefined embed.
//
// yjs writes such an attribute as a format marker of its own (V2 tag 127;
// V1 cannot write it, JSON.stringify(undefined) is undefined and
// writeVarString throws), and its formatting code treats undefined
// differently at each place it reads a value: most reads coalesce it to
// null, insertText turns a requested undefined for a key in effect into
// null, formatText's walk treats a requested undefined as not requested,
// insertNegatedAttributes steps over a marker whose value is undefined for
// any key, cleanupFormattingGap compares it raw, and the attributes in
// effect keep the key (toDelta and event deltas carry it).
//
// A scenario is a writer's transactions (client 1), after optional setup
// updates from a peer (client 2). Recorded, in order: every V2 update event
// and every text event delta of the writer; then its V2 state, delta and
// string; then the same for a receiver (client 99) that applies every V2
// update the writer emitted. undefined is written as {"$undefined": true}
// in ops and deltas, so it survives JSON. The Go test
// (format_undefined_fixture_test.go) replays the writer through ygo's API
// and the receiver through ApplyUpdateV2 and requires the same bytes and
// deltas.
//
// Run: node gen-format-undefined.mjs (after npm install in this directory).

import { writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";
import * as Y from "yjs";

const here = dirname(fileURLToPath(import.meta.url));
const outPath = process.env.OUT || resolve(here, "..", "format-undefined-fixtures.json");
const toHex = (u8) => Array.from(u8, (b) => b.toString(16).padStart(2, "0")).join("");
const fromHex = (h) => Uint8Array.from(Buffer.from(h, "hex"));

const U = { $undefined: true };
const isU = (v) => v !== null && typeof v === "object" && !Array.isArray(v) && Object.keys(v).length === 1 && v.$undefined === true;
// revive turns the marker into undefined, keeping keys whose value it is.
const revive = (v) => {
  if (isU(v)) return undefined;
  if (Array.isArray(v)) return v.map(revive);
  if (v !== null && typeof v === "object") {
    const o = {};
    for (const k of Object.keys(v)) o[k] = revive(v[k]);
    return o;
  }
  return v;
};
// mark turns undefined into the marker, keeping keys whose value it is.
const mark = (v) => {
  if (v === undefined) return U;
  if (Array.isArray(v)) return v.map(mark);
  if (v !== null && typeof v === "object" && !(v instanceof Uint8Array)) {
    const o = {};
    for (const k of Object.keys(v)) o[k] = mark(v[k]);
    return o;
  }
  return v;
};

const ins = (index, text, attrs) => (attrs === undefined ? { op: "insert", index, text } : { op: "insert", index, text, attrs });
const embed = (index, value, attrs) => (attrs === undefined ? { op: "embed", index, embed: value } : { op: "embed", index, embed: value, attrs });
const fmt = (index, length, attrs) => ({ op: "format", index, length, attrs });
const del = (index, length) => ({ op: "delete", index, length });
const delta = (ops) => ({ op: "delta", delta: ops });

function run(text, op) {
  switch (op.op) {
    case "insert":
      return "attrs" in op ? text.insert(op.index, op.text, revive(op.attrs)) : text.insert(op.index, op.text);
    case "embed":
      return "attrs" in op ? text.insertEmbed(op.index, revive(op.embed), revive(op.attrs)) : text.insertEmbed(op.index, revive(op.embed));
    case "format":
      return text.format(op.index, op.length, revive(op.attrs));
    case "delete":
      return text.delete(op.index, op.length);
    case "delta":
      return text.applyDelta(revive(op.delta));
    default:
      throw new Error(`op ${op.op}`);
  }
}

// peerUpdate runs ops on a fresh peer (client 2) and returns its V2 state.
function peerUpdate(fn) {
  const p = new Y.Doc();
  p.clientID = 2;
  fn(p.getText("t"), p);
  return Y.encodeStateAsUpdateV2(p);
}

// An undefined embed, which the API cannot insert, from a peer: built as
// the item yjs would build for insertEmbed.
const undefinedEmbed = peerUpdate((t, p) => {
  t.insert(0, "ab");
  Y.transact(p, (tr) => {
    const left = t._start;
    new Y.Item(Y.createID(2, Y.getState(p.store, 2)), left, left.lastId, null, null, t, null, new Y.ContentEmbed(undefined)).integrate(tr, 0);
  });
});

// eventDelta is the event's delta, or {"$throws": true} where yjs's delta
// getter throws (it reads insert.length of an undefined embed).
const eventDelta = (e) => {
  try {
    return mark(e.delta);
  } catch {
    return { $throws: true };
  }
};

const scenarios = [];
function scenario(name, { setup = [], txns, undo = false }) {
  const w = new Y.Doc();
  w.clientID = 1;
  const t = w.getText("t");
  const um = undo ? new Y.UndoManager(t, { captureTimeout: 0 }) : null;
  const wEvents = [];
  const wUpdates = [];
  t.observe((e) => wEvents.push(eventDelta(e)));
  w.on("updateV2", (u) => wUpdates.push(toHex(u)));
  for (const s of setup) Y.applyUpdateV2(w, s);
  for (const txn of txns) {
    if (txn === "undo") um.undo();
    else if (txn === "redo") um.redo();
    else Y.transact(w, () => { for (const op of txn) run(t, op); });
  }
  const r = new Y.Doc();
  r.clientID = 99;
  const rt = r.getText("t");
  const rEvents = [];
  const rUpdates = [];
  rt.observe((e) => rEvents.push(eventDelta(e)));
  r.on("updateV2", (u) => rUpdates.push(toHex(u)));
  for (const u of wUpdates) Y.applyUpdateV2(r, fromHex(u));
  let v1 = null;
  try { v1 = toHex(Y.encodeStateAsUpdate(w)); } catch { v1 = null; } // null: yjs cannot write it in V1
  scenarios.push({
    name,
    setup: setup.map(toHex),
    txns,
    undo,
    writer: { events: wEvents, updates: wUpdates, state: toHex(Y.encodeStateAsUpdateV2(w)), delta: mark(t.toDelta()), string: t.toString(), v1 },
    receiver: { events: rEvents, updates: rUpdates, state: toHex(Y.encodeStateAsUpdateV2(r)), delta: mark(rt.toDelta()), string: rt.toString() },
  });
}

const B = { bold: true };

// Insert (plan rows 4, 5, 13).
scenario("insert with an undefined attribute", { txns: [[ins(0, "abc", { bold: U })]] });
scenario("insert with an undefined attribute inside a run of it", { txns: [[ins(0, "abcdef", B)], [ins(3, "X", { bold: U })]] });
scenario("insert with an undefined other attribute inside a run", { txns: [[ins(0, "abcdef", B)], [ins(3, "X", { italic: U })]] });
scenario("insert inheriting an undefined attribute", { txns: [[ins(0, "abc", { bold: U })], [ins(2, "X")]] });
scenario("insert after an undefined marker without attributes", { txns: [[ins(0, "abc", { bold: U })], [ins(3, "X", {})]] });

// Format (rows 3, 4, 6, 7).
scenario("format undefined over plain text", { txns: [[ins(0, "abcdef")], [fmt(1, 3, { bold: U })]] });
scenario("format undefined over a run", { txns: [[ins(0, "abcdef", B)], [fmt(1, 3, { bold: U })]] });
scenario("format undefined twice", { txns: [[ins(0, "abcdef")], [fmt(1, 3, { bold: U })], [fmt(1, 3, { bold: U })]] });
scenario("format a defined and an undefined attribute", { txns: [[ins(0, "abcdef")], [fmt(0, 6, { bold: true, italic: U })]] });
scenario("format next to an undefined marker of another key", { txns: [[ins(0, "abcdef")], [fmt(3, 3, { italic: U })], [fmt(0, 3, B)]] });
// insertNegatedAttributes after new content: the marker right after it
// sets another key to undefined; yjs steps over it (an absent key reads
// undefined) and closes bold after it.
scenario("insert before an undefined marker of another key", { txns: [[ins(0, "abcdef")], [fmt(3, 3, { italic: U })], [ins(3, "X", B)]] });
scenario("format across an undefined marker", { txns: [[ins(0, "abcdef")], [fmt(2, 2, { bold: U })], [fmt(0, 6, B)]] });
scenario("format null over an undefined marker", { txns: [[ins(0, "abcdef")], [fmt(1, 3, { bold: U })], [fmt(0, 6, { bold: null })]] });

// Delete and the cleanup of format markers (rows 9).
scenario("delete across undefined markers", { txns: [[ins(0, "abcdef")], [fmt(2, 2, { bold: U })], [del(1, 4)]] });
scenario("delete a run formatted undefined", { txns: [[ins(0, "ab"), ins(2, "cd", { bold: U }), ins(4, "ef")], [del(2, 2)]] });
scenario("delete between an undefined and a defined marker", { txns: [[ins(0, "ab", { bold: U }), ins(2, "cd", B), ins(4, "ef")], [del(1, 2)]] });

// Delta (rows 10, 11, 12).
scenario("applyDelta with undefined attributes", { txns: [[delta([{ insert: "abc", attributes: { bold: U } }, { insert: "d" }])], [delta([{ retain: 1 }, { retain: 2, attributes: { bold: U, italic: true } }])]] });
scenario("event deltas over several transactions", { txns: [[ins(0, "abcdef")], [fmt(1, 2, { bold: U })], [fmt(0, 6, B)], [del(2, 1)], [ins(1, "Z", { bold: U })]] });

// From a peer, then local edits around it (rows 1, 2, 5, 7, the cleanup after a remote transaction).
const peerRun = peerUpdate((t) => { t.insert(0, "abcdef"); t.format(1, 3, { bold: undefined }); t.format(2, 3, { italic: true }); });
scenario("undefined markers from a peer, then inserts", { setup: [peerRun], txns: [[ins(2, "X")], [ins(5, "Y", B)], [ins(0, "Z", { bold: U })]] });
scenario("undefined markers from a peer, then a format", { setup: [peerRun], txns: [[fmt(0, 6, { bold: true, italic: U })]] });
scenario("undefined markers from a peer, then deletes", { setup: [peerRun], txns: [[del(2, 2)], [del(0, 1)]] });

// Embeds (row 15).
scenario("an embed with an undefined attribute", { txns: [[ins(0, "ab"), embed(1, { img: "x" }, { link: U })]] });
scenario("an undefined embed from a peer", { setup: [undefinedEmbed], txns: [[ins(3, "c", B)]] });

// Undo and redo of a format with undefined.
scenario("undo and redo a format with undefined", { undo: true, txns: [[ins(0, "abcdef")], [fmt(1, 3, { bold: U })], "undo", "redo"] });
scenario("undo an insert with undefined", { undo: true, txns: [[ins(0, "abc", B)], [ins(1, "X", { bold: U, italic: U })], "undo"] });

// From the plan review: ychange, a key toDelta drops at the next string
// unless its value is undefined; the cursor a delete leaves for the insert
// after it in one applyDelta (yjs: plain "zy"); an insert of undefined in
// applyDelta, which yjs skips for the op's retain.
scenario("ychange set to undefined", { txns: [[ins(0, "x", { ychange: U })], [ins(1, "y")]] });
scenario("applyDelta delete then insert with undefined", { txns: [[ins(0, "x", { b: U }), ins(1, "y", {})], [delta([{ delete: 1 }, { insert: "z", attributes: { b: U } }])]] });
scenario("applyDelta insert of undefined is a retain", { txns: [[ins(0, "abc")], [delta([{ insert: U, retain: 2, attributes: B }])]] });

// Snapshots: yjs restores them through V2 (createDocFromSnapshot), so an
// undefined marker survives. Recorded: the restored document's V2 state
// and delta.
const snapshots = [];
function snapshotScenario(name, before, after) {
  const w = new Y.Doc({ gc: false });
  w.clientID = 1;
  const t = w.getText("t");
  for (const txn of before) Y.transact(w, () => { for (const op of txn) run(t, op); });
  const snap = Y.snapshot(w);
  for (const txn of after) Y.transact(w, () => { for (const op of txn) run(t, op); });
  const r = Y.createDocFromSnapshot(w, snap);
  snapshots.push({ name, before, after, state: toHex(Y.encodeStateAsUpdateV2(r)), delta: mark(r.getText("t").toDelta()) });
}
snapshotScenario("a snapshot holding an undefined marker", [[ins(0, "abcdef")], [fmt(1, 3, { bold: U })]], [[fmt(0, 6, B)], [del(2, 2)]]);

// The fixtures depend on V8's two-element sort nowhere, but on JSON keeping
// key order and on Object.keys listing keys whose value is undefined.
if (Object.keys({ a: undefined }).length !== 1) throw new Error("Object.keys drops undefined keys");
writeFileSync(outPath, JSON.stringify({ generator: "gen-format-undefined.mjs (yjs 13.6.33)", scenarios, snapshots }, null, 2) + "\n");
console.log(`wrote ${scenarios.length} scenarios to ${outPath}`);
