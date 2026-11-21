// Generates testdata/search-marker-fixtures.json: yjs 13.6.33's search
// markers on a text whose root was typed after formatted text arrived.
//
// yjs turns a type's search markers off when a format marker integrates
// into it (ContentFormat.integrate), but Doc.get builds a fresh YText over a
// root that already holds format markers, with its markers on. Until the
// next format marker integrates, inserts, embeds without attributes and
// deletes start at a search marker, with only the attributes of the markers
// passed from there, so which markers exist decides where text goes. The
// update-event fixtures check the bytes that come out; these check the
// marker list itself: after every op and every commit, the markers in array
// order ([client, clock, index]: the item each points at and its index) or
// null once they are off, and the items whose marker flag is set
// ([client, clock]), in list order.
//
// Each scenario has a writer (client 3141592653) that builds the text and
// a receiver (client 271828182) that applies the writer's state while its
// root is untyped, reads it as text (or XmlText), and runs txns: each a list
// of ops in one transaction. A "remote" op applies a writer's op to the
// writer and sends the receiver the writer's diff, in a transaction of its
// own. The Go test (search_marker_fixture_test.go) replays the same through
// ygo's API and compares the dumps.
//
// Run: node gen-search-markers.mjs (after npm install in this directory).

import { writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";
import * as Y from "yjs";

const here = dirname(fileURLToPath(import.meta.url));
const outPath = process.env.OUT || resolve(here, "..", "search-marker-fixtures.json");
const toHex = (u8) => Array.from(u8, (b) => b.toString(16).padStart(2, "0")).join("");
const WRITER = 3141592653;
const RECEIVER = 271828182;

const textOf = (d, kind) => (kind === "xmltext" ? d.get("t", Y.XmlText) : d.getText("t"));

const dump = (t) => {
  const markers = t._searchMarker === null ? null : t._searchMarker.map((m) => [m.p.id.client, m.p.id.clock, m.index]);
  const marked = [];
  for (let it = t._start; it !== null; it = it.right) if (it.marker) marked.push([it.id.client, it.id.clock]);
  return { markers, marked };
};

const copy = (v) => (v === undefined ? undefined : JSON.parse(JSON.stringify(v)));

const runOp = (t, op) => {
  switch (op.op) {
    case "insert": return "attrs" in op ? t.insert(op.index, op.text, copy(op.attrs)) : t.insert(op.index, op.text);
    case "delete": return t.delete(op.index, op.len);
    case "embed": return "attrs" in op ? t.insertEmbed(op.index, copy(op.value), copy(op.attrs)) : t.insertEmbed(op.index, copy(op.value));
    case "format": return t.format(op.index, op.len, copy(op.attrs));
    case "delta": return t.applyDelta(copy(op.delta));
    case "setAttr": return t.setAttribute(op.key, copy(op.value));
    default: throw new Error(`unknown op ${op.op}`);
  }
};

const scenarios = [];
function scenario(name, kind, build, txns) {
  const w = new Y.Doc();
  w.clientID = WRITER;
  const wt = textOf(w, kind);
  build(wt, w);
  const state = Y.encodeStateAsUpdate(w);
  const r = new Y.Doc();
  r.clientID = RECEIVER;
  Y.applyUpdate(r, state, "remote");
  const rt = textOf(r, kind);
  const steps = [{ typed: dump(rt) }];
  for (const ops of txns) {
    if (ops.remote) {
      w.transact(() => runOp(wt, ops.remote));
      const u = Y.encodeStateAsUpdate(w, Y.encodeStateVector(r));
      Y.applyUpdate(r, u, "remote");
      steps.push({ remote: ops.remote, update: toHex(u), after: dump(rt) });
      continue;
    }
    const after = [];
    r.transact(() => {
      for (const op of ops) {
        if (op.op === "insert" || op.op === "embed") {
          const len = rt.length;
          if (op.index > len) op.index = len;
        }
        if (op.op === "delete" && op.index + op.len > rt.length) {
          op.len = Math.max(0, rt.length - op.index);
        }
        runOp(rt, op);
        after.push(dump(rt));
      }
    });
    steps.push({ ops, after, committed: dump(rt) });
  }
  scenarios.push({
    name,
    kind,
    writer: WRITER,
    receiver: RECEIVER,
    state: toHex(state),
    steps,
    final: { text: rt.toDelta(), state: toHex(Y.encodeStateAsUpdate(r)) },
  });
}

const ins = (index, text, attrs) => ({ op: "insert", index, text, ...(attrs !== undefined ? { attrs } : {}) });
const del = (index, len) => ({ op: "delete", index, len });
const embed = (index, value, attrs) => ({ op: "embed", index, value, ...(attrs !== undefined ? { attrs } : {}) });
const fmt = (index, len, attrs) => ({ op: "format", index, len, attrs });
const remote = (op) => ({ remote: op });

const plainThenBold = (t) => { t.insert(0, "abcd"); t.format(2, 2, { bold: true }); };
const boldInOneCall = (t) => { t.insert(0, "ab"); t.insert(2, "cdef", { bold: true }); t.insert(6, "gh"); };
const longRich = (t, d) => {
  d.transact(() => t.insert(0, "the quick brown fox jumps over the lazy dog ".repeat(6)));
  for (let i = 0; i < 12; i++) d.transact(() => t.format(5 + i * 20, 7, i % 2 ? { bold: true } : { italic: true, size: i }));
};

scenario("marker-left-by-earlier-insert", "text", plainThenBold, [[ins(3, "Q")], [ins(2, "Z")]]);
scenario("rewind-to-run-start", "text", plainThenBold, [[ins(2, "Q"), ins(3, "R"), ins(4, "S")], [ins(2, "Z")], [ins(4, "Q"), ins(5, "R"), ins(6, "S")], [ins(3, "Y")]]);
// A format that changes nothing splits "xy"; the insert's marker lands on y,
// which merges back into x at commit: the marker moves to x, index less x's
// length, and x's marker flag stays off (Item.mergeWith).
scenario("merge-repoints", "text", (t) => { t.insert(0, "xy"); t.format(0, 2, { bold: true }); }, [[fmt(1, 1, { bold: true }), ins(2, "Q")], [ins(1, "Z")], [ins(2, "W")]]);
// A format with {} changes nothing but splits the items at the range's ends,
// which moves the markers (a marker stays on the left half and moves again
// when the halves merge).
scenario("empty-format-splits", "text", (t) => { t.insert(0, "abcd"); t.format(1, 2, { bold: true }); }, [[fmt(2, 1, {}), ins(3, "Q")], remote(ins(4, "e")), [ins(3, "B")], [ins(1, "D")], [fmt(0, 5, {})], [ins(4, "W")]]);
// A remote clear empties the list but leaves the items' marker flags, which
// then decide which markers updateMarkerChanges drops.
scenario("stale-flag-after-remote-clear", "xmltext", plainThenBold, [[ins(4, "Q")], remote({ op: "setAttr", key: "k", value: "v" }), [ins(4, "X")], [ins(2, "Z")]]);
scenario("split-of-marked-item", "text", boldInOneCall, [[ins(4, "Q")], [ins(5, "Z")], [ins(2, "Y")], [ins(7, "W")], [del(3, 2)], [ins(3, "V")]]);
scenario("deletes", "text", plainThenBold, [[ins(3, "Q")], [del(0, 1)], [ins(1, "Z")], [del(2, 2)], [ins(2, "Y")], [del(0, 3)]]);
scenario("embeds", "text", plainThenBold, [[ins(3, "Q")], [embed(2, { img: 1 })], [embed(2, { img: 2 }, {})], [embed(5, { img: 3 }, { bold: true })]]);
scenario("delta", "text", plainThenBold, [[ins(3, "Q")], [{ op: "delta", delta: [{ retain: 1 }, { insert: "D" }] }], [ins(3, "Z")], [{ op: "delta", delta: [{ delete: 2 }] }], [ins(1, "Y")]]);
scenario("format-turns-off", "text", plainThenBold, [[ins(3, "Q")], [ins(2, "Z")], [fmt(0, 1, { italic: true })], [ins(2, "Y")], [del(1, 1)]]);
scenario("remote-clears", "text", plainThenBold, [[ins(3, "Q")], remote(ins(0, "e")), [ins(2, "Z")], [ins(3, "Y")], remote(ins(1, "f")), [ins(4, "X")]]);
scenario("xmltext", "xmltext", plainThenBold, [[ins(3, "Q")], [ins(2, "Z")], [del(1, 2)]]);
scenario("cap", "text", plainThenBold, [Array.from({ length: 90 }, (_, i) => ins(i % 2 ? 4 + i : 2 + (i % 7), String(i % 10))), [ins(2, "Z")], [ins(50, "Y")], [del(10, 5)], [ins(10, "X")]]);

// Chunks appended one per transaction, each two units longer than the one
// before and followed by one unit at the start: the extra unit takes a
// clock, so a chunk neither merges with the one before nor lets findMarker
// step back over it. Each append lands a chunk past the marker the one
// before left, and a chunk longer than the average is farther than
// length/80, so the markers fill all 80 and later ones overwrite the least
// recently used.
scenario("lru-eviction", "text", plainThenBold, (() => {
  let len = 4;
  return Array.from({ length: 100 }, (_, i) => {
    const n = 20 + 2 * i;
    const ops = [ins(len, String.fromCharCode(97 + (i % 26)).repeat(n)), ins(0, "-")];
    len += n + 1;
    return ops;
  });
})());

// The same, after 1000 units typed backwards, with inserts deep inside them
// once the markers are nearly full: each starts at the lowest marker, one
// of the oldest, and goes on to make a marker of its own, so the marker it
// used is refreshed (refreshMarkerTimestamp) and the eviction that follows
// spares it.
const typedBackwards = (t, d) => {
  d.transact(() => { for (let i = 0; i < 1000; i++) t.insert(0, "x"); });
  t.format(500, 10, { bold: true });
};
scenario("lru-refresh", "text", typedBackwards, (() => {
  let len = 1000;
  return Array.from({ length: 100 }, (_, i) => {
    const n = 20 + 2 * i;
    const ops = [ins(len, String.fromCharCode(97 + (i % 26)).repeat(n)), ins(0, "-")];
    len += n + 1;
    if (i >= 74 && i % 3 === 0) {
      ops.push(ins(100 + 7 * i, "R"));
      len += 1;
    }
    return ops;
  });
})());

// Seeded random sessions on a long formatted text: inserts and deletes at
// random places, some embeds, transactions of one to four ops.
function rng(seed) {
  let s = seed >>> 0;
  return () => {
    s = (s + 0x6d2b79f5) >>> 0;
    let t = s;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}
for (const seed of [1, 2, 3, 4]) {
  const rand = rng(seed);
  const pick = (n) => Math.floor(rand() * n);
  let len = 264;
  const txns = [];
  for (let k = 0; k < 60; k++) {
    const ops = [];
    const n = 1 + pick(4);
    for (let j = 0; j < n; j++) {
      const r = rand();
      if (r < 0.6 || len < 10) {
        // A run of inserts next to each other, the way typing goes.
        const at = pick(len + 1);
        const text = "xyz".slice(0, 1 + pick(3));
        ops.push(ins(at, text));
        len += text.length;
      } else if (r < 0.9) {
        const at = pick(len - 3);
        const n2 = 1 + pick(3);
        ops.push(del(at, n2));
        len -= n2;
      } else {
        ops.push(embed(pick(len + 1), { e: k }));
        len += 1;
      }
    }
    txns.push(ops);
  }
  scenario(`random-${seed}`, "text", longRich, txns);
}

// One step per line, so a regenerated file diffs by step.
const lines = scenarios.map(({ steps, ...rest }) => {
  const head = JSON.stringify(rest);
  return `${head.slice(0, -1)},"steps":[\n${steps.map((st) => "  " + JSON.stringify(st)).join(",\n")}\n]}`;
});
writeFileSync(outPath, `{"yjs":"13.6.33","scenarios":[\n${lines.join(",\n")}\n]}\n`);
console.log(`wrote ${scenarios.length} scenarios to ${outPath}`);
