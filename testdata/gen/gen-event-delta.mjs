// Generates testdata/event-delta-fixtures.json: what a yjs 13.6.33 receiver
// does with concurrent formatting, depending on whether anybody reads the
// text's event deltas.
//
// yjs's YTextEvent.delta getter deletes a format marker the walk finds
// redundant (an added marker equal to the attribute already in effect, or
// an older marker equal to the attribute an added one set), in a local
// transaction of its own. The after-transaction cleanup
// (cleanupYTextAfterTransaction) runs only for a root typed as a text. So
// one set of updates leaves a receiver in one of three states: untyped
// root (mode "untyped"), typed and never read ("typed"), read by a shallow
// observer ("observed") or by a deep observer of the parent ("deep").
//
// A scenario: a base document written by client 50, then peers (clients 1,
// 2, ...) that each start from the base and edit concurrently. A receiver
// (client 99) applies the base and each peer's update, in order. Recorded
// per mode: every V2 update event the receiver emits (local or not), every
// delta its observers read, its final V2 state. undefined is written as
// {"$undefined": true}. The Go test (event_delta_fixture_test.go) builds
// the same receiver in ygo and requires the same.
//
// Run: node gen-event-delta.mjs (after npm install in this directory).

import { writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";
import * as Y from "yjs";

const here = dirname(fileURLToPath(import.meta.url));
const outPath = process.env.OUT || resolve(here, "..", "event-delta-fixtures.json");
const toHex = (u8) => Array.from(u8, (b) => b.toString(16).padStart(2, "0")).join("");

const U = { $undefined: true };
const isU = (v) => v !== null && typeof v === "object" && !Array.isArray(v) && Object.keys(v).length === 1 && v.$undefined === true;
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
const mark = (v) => {
  if (v === undefined) return U;
  if (Array.isArray(v)) return v.map(mark);
  if (v !== null && typeof v === "object" && !(v instanceof Uint8Array) && !(v instanceof Y.AbstractType)) {
    const o = {};
    for (const k of Object.keys(v)) o[k] = mark(v[k]);
    return o;
  }
  return v;
};

// Shapes: where the edited texts live.
//   root:   root texts "t" (and "u"); ops name the text.
//   map:    a text at key "k" of root map "m"; the deep observer is on "m".
//   xml:    an XmlText, child 0 of root XmlFragment "x".
//   depth:  texts at two depths: "k" of root map "m", and "k2" of map "n"
//           at "n" of "m"; the deep observer is on "m".
const shapes = {
  root: {
    setup: (d) => {},
    text: (d, name = "t") => d.getText(name),
    root: (d) => null,
  },
  map: {
    setup: (d) => d.getMap("m").set("k", new Y.Text()),
    text: (d) => d.getMap("m").get("k"),
    root: (d) => d.getMap("m"),
  },
  xml: {
    setup: (d) => d.getXmlFragment("x").insert(0, [new Y.XmlText()]),
    text: (d) => d.getXmlFragment("x").get(0),
    root: (d) => d.getXmlFragment("x"),
  },
  depth: {
    setup: (d) => {
      const m = d.getMap("m");
      m.set("k", new Y.Text());
      const n = new Y.Map();
      m.set("n", n);
      n.set("k2", new Y.Text());
    },
    text: (d, name) => (name === "k2" ? d.getMap("m").get("n").get("k2") : d.getMap("m").get("k")),
    root: (d) => d.getMap("m"),
  },
};

const ins = (index, text, attrs, target) => ({ op: "insert", index, text, ...(attrs === undefined ? {} : { attrs }), ...(target ? { target } : {}) });
const fmt = (index, length, attrs, target) => ({ op: "format", index, length, attrs, ...(target ? { target } : {}) });
const del = (index, length, target) => ({ op: "delete", index, length, ...(target ? { target } : {}) });
const attr = (name, value, target) => ({ op: "attr", name, value, ...(target ? { target } : {}) });

function run(text, op) {
  switch (op.op) {
    case "insert":
      return "attrs" in op ? text.insert(op.index, op.text, revive(op.attrs)) : text.insert(op.index, op.text);
    case "format":
      return text.format(op.index, op.length, revive(op.attrs));
    case "delete":
      return text.delete(op.index, op.length);
    case "attr":
      return text.setAttribute(op.name, revive(op.value));
    default:
      throw new Error("unknown op " + op.op);
  }
}

// build runs a scenario's writers and returns the updates a receiver gets.
function build(sc) {
  const shape = shapes[sc.shape];
  const base = new Y.Doc();
  base.clientID = 50;
  base.transact(() => shape.setup(base));
  for (const op of sc.base) run(shape.text(base, op.target), op);
  const b0 = Y.encodeStateAsUpdateV2(base);
  const updates = [toHex(b0)];
  const fromPeers = sc.peers.map((ops, i) => {
    const p = new Y.Doc();
    p.clientID = i + 1;
    Y.applyUpdateV2(p, b0);
    const sv = Y.encodeStateVector(p);
    for (const op of ops) run(shape.text(p, op.target), op);
    return Y.encodeStateAsUpdateV2(p, sv);
  });
  // merged: the peers' updates arrive as one (a server that merges them),
  // so one transaction adds all their markers.
  if (sc.merged) updates.push(toHex(Y.mergeUpdatesV2(fromPeers)));
  else for (const u of fromPeers) updates.push(toHex(u));
  return updates;
}

// Receiver modes:
//   untyped   the root never typed
//   typed     typed, no observer
//   silent    an observer that never reads the delta
//   observed  an observer reading every delta (and the changed keys)
//   oneOfTwo  two observers, the second reads
//   deep      a deep observer on the parent reading every text delta
//   undo      observed, with an UndoManager on the text (remote origin not
//             tracked, no grouping), then undo, redo, undo
// xml has no "deep": ygo has no XmlFragment.ObserveDeep yet.
const MODES = {
  root: ["untyped", "typed", "silent", "observed", "oneOfTwo"],
  map: ["typed", "observed", "deep"],
  xml: ["typed", "observed"],
  depth: ["typed", "deep"],
};

const keysOf = (e) => [...e.keys.entries()].sort(([a], [b]) => (a < b ? -1 : 1)).map(([k, c]) => [k, c.action, mark(c.oldValue)]);

// receive applies the updates to a receiver in a mode and records what it
// emits, its after-transaction trace and what it reads.
function receive(sc, updates, mode) {
  const shape = shapes[sc.shape];
  const r = new Y.Doc();
  r.clientID = 99;
  const events = [];
  const trace = [];
  const deltas = [];
  const stacks = [];
  r.on("updateV2", (u, origin, d, tr) => events.push({ update: toHex(u), local: tr.local }));
  const names = sc.shape === "root" ? [...new Set([...sc.base, ...sc.peers.flat()].map((op) => op.target || "t"))].sort() : [null];
  const reader = (name) => (e) => deltas.push({ text: name ?? "", delta: mark(e.delta), keys: keysOf(e) });
  const observe = (name, t) => {
    if (mode === "silent") t.observe(() => {});
    if (mode === "observed" || mode === "undo") t.observe(reader(name));
    if (mode === "oneOfTwo") {
      t.observe(() => {});
      t.observe(reader(name));
    }
  };
  if (mode !== "untyped") {
    if (sc.shape === "root") for (const n of names) observe(n, r.getText(n));
    else shape.root(r);
  }
  if (mode === "deep") {
    shape.root(r).observeDeep((evs) => {
      for (const e of evs) if (e instanceof Y.YTextEvent) deltas.push({ text: "", path: e.path, delta: mark(e.delta), keys: keysOf(e) });
    });
  }
  const um = mode === "undo" ? new Y.UndoManager(r.getText("t"), { captureTimeout: 0 }) : null;
  // The trace starts with the first update: getXmlFragment's own empty
  // transaction (its prelim insert) is not receiving.
  r.on("afterTransaction", (tr) => {
    let deleted = 0;
    tr.deleteSet.clients.forEach((rs) => rs.forEach((x) => (deleted += x.len)));
    const origin = tr.origin === null ? null : typeof tr.origin === "string" ? tr.origin : "undo-manager";
    trace.push({ origin, local: tr.local, deleted });
  });
  updates.forEach((u, i) => {
    Y.applyUpdateV2(r, Uint8Array.from(Buffer.from(u, "hex")), "remote");
    // A nested text exists at the receiver once the base arrived.
    if (i === 0 && sc.shape !== "root") observe(null, shape.text(r));
  });
  if (um) {
    const step = (name) => stacks.push({ step: name, undo: um.undoStack.length, redo: um.redoStack.length, state: toHex(Y.encodeStateAsUpdateV2(r)) });
    step("received");
    for (const s of ["undo", "redo", "undo"]) {
      um[s]();
      step(s);
    }
  }
  return { mode, events, trace, deltas, ...(um ? { stacks } : {}), state: toHex(Y.encodeStateAsUpdateV2(r)) };
}

const scenarios = [];
function scenario(name, sc, extraModes = []) {
  sc.name = name;
  const updates = build(sc);
  scenarios.push({ ...sc, updates, receivers: [...MODES[sc.shape], ...extraModes].map((m) => receive(sc, updates, m)) });
}

const B = { bold: true };
scenario("the same character bolded by two peers", { shape: "root", base: [ins(0, "x")], peers: [[fmt(0, 1, B)], [fmt(0, 1, B)]] }, ["undo"]);
scenario("three peers bold the same range", { shape: "root", base: [ins(0, "abc")], peers: [[fmt(0, 3, B)], [fmt(0, 3, B)], [fmt(1, 2, B)]] });
scenario("a bold over a range holding an older bold run", { shape: "root", base: [ins(0, "abcd"), fmt(1, 2, B)], peers: [[fmt(0, 4, B)]] });
scenario("an older bold run made redundant concurrently", { shape: "root", base: [ins(0, "abcd"), fmt(2, 1, B)], peers: [[fmt(0, 4, B)], [fmt(1, 3, B)]] });
scenario("two texts in one update, each with a duplicate", {
  shape: "root",
  base: [ins(0, "x"), ins(0, "y", undefined, "u")],
  peers: [[fmt(0, 1, B), fmt(0, 1, B, "u")], [fmt(0, 1, B), fmt(0, 1, B, "u")]],
});
scenario("a duplicate and a deletion next to it", { shape: "root", base: [ins(0, "xyz")], peers: [[fmt(1, 1, B)], [fmt(1, 1, B), del(2, 1)]] });
scenario("values that differ", { shape: "root", base: [ins(0, "x")], peers: [[fmt(0, 1, B)], [fmt(0, 1, { bold: "x" })]] });
scenario("bold and italic from different peers", { shape: "root", base: [ins(0, "xy")], peers: [[fmt(0, 2, { bold: true, italic: true })], [fmt(0, 1, { italic: true }), fmt(1, 1, B)]] });
scenario("an undefined attribute twice", { shape: "root", base: [ins(0, "x")], peers: [[fmt(0, 1, { bold: U })], [fmt(0, 1, { bold: U })]] });
scenario("an unbold over a bold run, twice", { shape: "root", base: [ins(0, "abc", B)], peers: [[fmt(0, 3, { bold: null })], [fmt(1, 1, { bold: null })]] });
scenario("a nested text bolded by two peers", { shape: "map", base: [ins(0, "x")], peers: [[fmt(0, 1, B)], [fmt(0, 1, B)]] });
scenario("a nested text: a bold over an older run", { shape: "map", base: [ins(0, "abcd"), fmt(1, 2, B)], peers: [[fmt(0, 4, B)]] });
scenario("texts at two depths under one deep observer, the deeper changed first", {
  shape: "depth",
  base: [ins(0, "x"), ins(0, "y", undefined, "k2")],
  peers: [[fmt(0, 1, B, "k2"), fmt(0, 1, B)], [fmt(0, 1, B, "k2"), fmt(0, 1, B)]],
});
scenario("a text attribute set with a duplicate marker", { shape: "root", base: [ins(0, "x")], peers: [[fmt(0, 1, B), attr("lang", "en")], [fmt(0, 1, B)]] });
scenario("a text attribute alone", { shape: "root", base: [ins(0, "x")], peers: [[attr("lang", "en")], [attr("lang", "de")]] });
scenario("an older bold run made redundant, undone and redone", { shape: "root", base: [ins(0, "abcd"), fmt(2, 1, B)], peers: [[fmt(0, 4, B)], [fmt(1, 3, B)]] }, ["undo"]);
scenario("an XmlText bolded by two peers", { shape: "xml", base: [ins(0, "x")], peers: [[fmt(0, 1, B)], [fmt(0, 1, B)]] });
scenario("an XmlText: a bold over an older run", { shape: "xml", base: [ins(0, "abcd"), fmt(1, 2, B)], peers: [[fmt(0, 4, B)], [fmt(2, 2, B)]] });

// A seeded search: random concurrent edits, kept when the observed
// receiver ends elsewhere than the typed one (the getter deleted
// something).
function mulberry32(a) {
  return () => {
    a |= 0;
    a = (a + 0x6d2b79f5) | 0;
    let t = Math.imul(a ^ (a >>> 15), 1 | a);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}
const rnd = mulberry32(14);
const pick = (a) => a[Math.floor(rnd() * a.length)];
const VALUES = [{ bold: true }, { bold: true }, { bold: null }, { italic: true }, { bold: true, italic: true }, { bold: "x" }];
function randomOps(len, n) {
  const ops = [];
  for (let i = 0; i < n; i++) {
    const r = rnd();
    if (r < 0.6 || len < 2) {
      const at = Math.floor(rnd() * Math.max(len, 1));
      const l = 1 + Math.floor(rnd() * Math.max(len - at, 1));
      if (len > 0) ops.push(fmt(at, Math.min(l, len - at), pick(VALUES)));
    } else if (r < 0.85) {
      const at = Math.floor(rnd() * (len + 1));
      ops.push(rnd() < 0.5 ? ins(at, "q", pick(VALUES)) : ins(at, "q"));
      len++;
    } else {
      const at = Math.floor(rnd() * len);
      ops.push(del(at, 1));
      len--;
    }
  }
  return ops;
}
let kept = 0;
for (let tries = 0; tries < 4000 && kept < 12; tries++) {
  const baseLen = 2 + Math.floor(rnd() * 4);
  const base = [ins(0, "abcdef".slice(0, baseLen))];
  if (rnd() < 0.6) base.push(fmt(Math.floor(rnd() * (baseLen - 1)), 1 + Math.floor(rnd() * (baseLen - 1)), pick(VALUES.slice(0, 2))));
  const peers = Array.from({ length: 2 + Math.floor(rnd() * 2) }, () => randomOps(baseLen, 1 + Math.floor(rnd() * 3)));
  const sc = { shape: "root", base, peers };
  let updates;
  try {
    updates = build(sc);
  } catch {
    continue;
  }
  const typed = receive(sc, updates, "typed");
  const observed = receive(sc, updates, "observed");
  if (typed.state !== observed.state) {
    kept++;
    scenario(`random ${kept} (seed 14, try ${tries})`, sc);
  }
}

// A second seeded search, delivered merged: kept when the getter deletes a
// marker right after content the same transaction inserted, where the
// deleted marker must not end the insert op (the walk sees it deleted).
function splitsAnInsert(sc, updates) {
  const r = new Y.Doc();
  r.clientID = 99;
  const t = r.getText("t");
  let found = false;
  t.observe((e) => {
    const del = Y.Item.prototype.delete;
    Y.Item.prototype.delete = function (tr) {
      const l = this.left;
      if (l && !l.deleted && e.adds(l) && l.content.constructor.name === "ContentString") found = true;
      return del.call(this, tr);
    };
    try {
      void e.delta;
    } finally {
      Y.Item.prototype.delete = del;
    }
  });
  for (const u of updates) Y.applyUpdateV2(r, Uint8Array.from(Buffer.from(u, "hex")), "remote");
  return found;
}
const rnd2 = mulberry32(1446);
const pick2 = (a) => a[Math.floor(rnd2() * a.length)];
let kept2 = 0;
for (let tries = 0; tries < 20000 && kept2 < 3; tries++) {
  const baseLen = 2 + Math.floor(rnd2() * 3);
  const base = [ins(0, "abcd".slice(0, baseLen))];
  if (rnd2() < 0.5) base.push(fmt(Math.floor(rnd2() * (baseLen - 1)), 1, { bold: true }));
  const peers = Array.from({ length: 2 + Math.floor(rnd2() * 2) }, () => {
    const ops = [];
    const n = 1 + Math.floor(rnd2() * 2);
    for (let i = 0; i < n; i++) {
      const at = Math.floor(rnd2() * (baseLen + 1));
      if (rnd2() < 0.5) ops.push(ins(at, pick2(["q", "qq"]), pick2([{ bold: true }, undefined])));
      else ops.push(fmt(Math.min(at, baseLen - 1), 1, { bold: true }));
    }
    return ops;
  });
  const sc = { shape: "root", base, peers, merged: true };
  let updates;
  try {
    updates = build(sc);
  } catch {
    continue;
  }
  if (splitsAnInsert(sc, updates)) {
    kept2++;
    scenario(`merged random ${kept2}, a marker deleted after inserted text (seed 1446, try ${tries})`, sc);
  }
}

writeFileSync(outPath, JSON.stringify({ generator: "gen-event-delta.mjs (yjs 13.6.33)", scenarios }, null, 2) + "\n");
console.log(`wrote ${scenarios.length} scenarios to ${outPath}`);
