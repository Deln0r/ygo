// Generates testdata/merge-update-fixtures.json: yjs 13.6.33's mergeUpdates
// and mergeUpdatesV2 of lists of updates, and diffUpdate / diffUpdateV2 of
// the merged update against several state vectors.
//
// yjs merges updates' bytes without integrating them (utils/updates.js
// mergeUpdatesV2): a reader per update over its structs in wire order, Skips
// dropped; each step the readers are sorted (client descending, clock
// ascending; at one clock a GC and an item compare as -1 both ways, so V8's
// sort decides) and the first writes; what is written is skipped in the
// others and what overlaps is cut; GC runs merge, items never; a section
// starts wherever the client changes; the delete sets are merged. diffUpdate
// copies an update's structs from the state vector on, the first one cut
// there, and its whole delete set. The Go test (merge_update_fixture_test.go)
// requires the same bytes from ygo.MergeUpdates, MergeUpdatesV2, DiffUpdate
// and DiffUpdateV2.
//
// Run: node gen-merge-updates.mjs (after npm install in this directory).

import { writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";
import * as Y from "yjs";

const here = dirname(fileURLToPath(import.meta.url));
const outPath = process.env.OUT || resolve(here, "..", "merge-update-fixtures.json");
const toHex = (u8) => Array.from(u8, (b) => b.toString(16).padStart(2, "0")).join("");

// A pair of the same update in V1 and V2.
const pair = (v1, v2) => ({ v1, v2 });
// edit runs fn in one transaction on doc and returns the update it made, V1
// and V2.
function edit(doc, fn) {
  const sv = Y.encodeStateVector(doc);
  doc.transact(fn);
  return pair(Y.encodeStateAsUpdate(doc, sv), Y.encodeStateAsUpdateV2(doc, sv));
}
const state = (doc, sv) => pair(Y.encodeStateAsUpdate(doc, sv), Y.encodeStateAsUpdateV2(doc, sv));
const raw = (v1) => pair(v1, Y.convertUpdateFormatV1ToV2(v1));
const newDoc = (client, gc = true) => {
  const d = new Y.Doc({ gc });
  d.clientID = client;
  return d;
};
const svOf = (entries) => Y.encodeStateVector(new Map(entries));

const scenarios = [];
function scenario(name, inputs, svs = []) {
  const v1 = inputs.map((p) => p.v1);
  const v2 = inputs.map((p) => p.v2);
  const m1 = Y.mergeUpdates(v1);
  const m2 = Y.mergeUpdatesV2(v2);
  const allSvs = [new Uint8Array([0]), ...svs];
  scenarios.push({
    name,
    inputs_v1: v1.map(toHex),
    inputs_v2: v2.map(toHex),
    merge_v1: toHex(m1),
    merge_v2: toHex(m2),
    diffs: allSvs.map((sv) => ({ sv: toHex(sv), v1: toHex(Y.diffUpdate(m1, sv)), v2: toHex(Y.diffUpdateV2(m2, sv)) })),
    // diffUpdate of each input as it is, against the first non-empty sv.
    input_diffs: inputs.map((p) => {
      const sv = svs[0] || new Uint8Array([0]);
      return { sv: toHex(sv), v1: toHex(Y.diffUpdate(p.v1, sv)), v2: toHex(Y.diffUpdateV2(p.v2, sv)) };
    }),
  });
}

// One client typing one character per transaction: yjs keeps each update's
// run; a document would squash them.
{
  const d = newDoc(1);
  const t = d.getText("t");
  const ups = [..."hello"].map((ch, i) => edit(d, () => t.insert(i, ch)));
  scenario("per-edit typing", ups, [svOf([[1, 2]]), svOf([[1, 5]])]);
  scenario("per-edit typing, reversed", [...ups].reverse(), [svOf([[1, 3]])]);
}

// Two clients editing concurrently, their updates interleaved.
{
  const a = newDoc(1);
  const b = newDoc(2);
  const ta = a.getText("t");
  const tb = b.getText("t");
  const ups = [];
  ups.push(edit(a, () => ta.insert(0, "ab")));
  Y.applyUpdate(b, ups[0].v1);
  ups.push(edit(b, () => tb.insert(1, "XY")));
  ups.push(edit(a, () => ta.insert(2, "cd")));
  ups.push(edit(b, () => tb.delete(0, 1)));
  Y.applyUpdate(a, ups[1].v1);
  ups.push(edit(a, () => ta.insert(0, "Z")));
  scenario("two clients interleaved", ups, [svOf([[1, 2], [2, 1]]), svOf([[2, 2]])]);
  scenario("two clients, b first", [ups[1], ups[3], ups[0], ups[2], ups[4]], [svOf([[1, 4]])]);
}

// The same clocks in two and three updates: a state, a later state, and a
// diff, in every order.
{
  const d = newDoc(1);
  const t = d.getText("t");
  edit(d, () => t.insert(0, "abc"));
  const s1 = state(d);
  edit(d, () => t.insert(3, "de"));
  const s2 = state(d);
  const delta = state(d, svOf([[1, 1]]));
  const ins = [s1, s2, delta];
  const perms = [[0, 1, 2], [0, 2, 1], [1, 0, 2], [1, 2, 0], [2, 0, 1], [2, 1, 0]];
  for (const p of perms) scenario(`overlapping states ${p.join("")}`, p.map((i) => ins[i]), [svOf([[1, 2]])]);
  scenario("overlapping states 01", [s1, s2]);
  scenario("overlapping states 10", [s2, s1]);
}

// A GC run and an item at one clock: the children of a nested array as
// items in one update and as GC (the array deleted and collected) in
// another, with two and three readers in every order.
{
  const d = newDoc(1);
  const a = d.getArray("a");
  edit(d, () => {
    const inner = new Y.Array();
    a.insert(0, [inner]);
    inner.insert(0, [1, 2, 3]);
  });
  const live = state(d);
  edit(d, () => a.delete(0, 1));
  const collected = state(d);
  const d2 = newDoc(3, false);
  Y.applyUpdate(d2, live.v1);
  d2.getArray("a").delete(0, 1);
  const deletedKept = state(d2);
  const ins = [live, collected, deletedKept];
  scenario("gc and item at one clock, live first", [live, collected]);
  scenario("gc and item at one clock, gc first", [collected, live]);
  const perms = [[0, 1, 2], [0, 2, 1], [1, 0, 2], [1, 2, 0], [2, 0, 1], [2, 1, 0]];
  for (const p of perms) scenario(`gc, item and deleted item ${p.join("")}`, p.map((i) => ins[i]));
  scenario("gc, item, gc", [collected, live, collected]);
  scenario("item, gc, item", [live, collected, live]);
  scenario("four readers", [live, collected, deletedKept, collected]);
}

// GC runs that touch and overlap, from raw updates (every number below 128,
// so each varuint is one byte).
{
  const gcRuns = (client, clock, ...lens) => new Uint8Array([1, lens.length, client, clock, ...lens.flatMap((n) => [0, n]), 0]);
  scenario("gc runs touching", [raw(gcRuns(1, 0, 2)), raw(gcRuns(1, 2, 3))]);
  scenario("gc runs overlapping", [raw(gcRuns(1, 0, 4)), raw(gcRuns(1, 2, 3)), raw(gcRuns(1, 1, 1))]);
  // One client in two sections of one update.
  const twoSections = new Uint8Array([2, 1, 1, 0, 0, 1, 1, 1, 5, 0, 1, 0]);
  scenario("a client in two sections", [raw(twoSections)]);
  scenario("a client in two sections, merged with another", [raw(twoSections), raw(gcRuns(1, 2, 2))]);
}

// Holes: an update whose predecessor is missing comes out after a Skip.
{
  const d = newDoc(1);
  const t = d.getText("t");
  const u1 = edit(d, () => t.insert(0, "ab"));
  edit(d, () => t.insert(2, "cd"));
  const u3 = edit(d, () => t.insert(4, "ef"));
  scenario("a hole between two updates", [u1, u3], [svOf([[1, 1]]), svOf([[1, 3]]), svOf([[1, 5]])]);
  scenario("a lone delta", [u3]);
  const e = newDoc(2);
  const u4 = edit(e, () => e.getText("t").insert(0, "zz"));
  scenario("a hole and another client", [u3, u4, u1], [svOf([[1, 4], [2, 1]])]);
}

// Map keys overwritten, nested types, rich text, embeds, deletes.
{
  const d = newDoc(1);
  const m = d.getMap("m");
  const t = d.getText("rich");
  const ups = [];
  ups.push(edit(d, () => m.set("k", 1)));
  ups.push(edit(d, () => m.set("k", 2)));
  ups.push(edit(d, () => m.set("nested", new Y.Map())));
  ups.push(edit(d, () => m.get("nested").set("x", "y")));
  ups.push(edit(d, () => t.insert(0, "bold", { bold: true })));
  ups.push(edit(d, () => t.insertEmbed(4, { img: "a.png" })));
  ups.push(edit(d, () => t.format(0, 2, { italic: true })));
  ups.push(edit(d, () => t.delete(1, 2)));
  ups.push(edit(d, () => m.delete("k")));
  scenario("maps, nested types, rich text", ups, [svOf([[1, 3]]), svOf([[1, 9]])]);
}

// Any values JavaScript writes: object keys out of sorted order, undefined,
// BigInt, -0, nested arrays.
{
  const d = newDoc(1);
  const a = d.getArray("a");
  const ups = [];
  ups.push(edit(d, () => a.push([{ z: 1, a: 2, "10": 3, "2": 4 }])));
  ups.push(edit(d, () => a.push([-0, { big: 2n ** 60n }])));
  ups.push(edit(d, () => d.getMap("m").set("u", undefined)));
  ups.push(edit(d, () => a.push([[1, [2, { b: null, a: true }]]])));
  ups.push(edit(d, () => a.delete(1, 1)));
  scenario("Any values", ups, [svOf([[1, 2]])]);
}

// 70 updates of one client, one character each, no GC: more readers than
// V8 sorts by insertion alone, with no tie its merges could break.
{
  const d = newDoc(1);
  const t = d.getText("t");
  const ups = [];
  for (let i = 0; i < 70; i++) ups.push(edit(d, () => t.insert(i, String.fromCharCode(97 + (i % 26)))));
  scenario("70 per-edit updates", ups, [svOf([[1, 35]])]);
  const shuffled = ups.map((u, i) => [((i * 37) % 70), u]).sort((x, y) => x[0] - y[0]).map(([, u]) => u);
  scenario("70 per-edit updates, shuffled", shuffled);
}

// Readers tied at one clock, a GC and an item, as many as V8 sorts by
// insertion (fewer than 8), by TimSort runs, and by TimSort merges with
// galloping (64 and more): which one writes is V8's order.
{
  const d = newDoc(1);
  const a = d.getArray("a");
  edit(d, () => {
    const inner = new Y.Array();
    a.insert(0, [inner]);
    inner.insert(0, [1, 2, 3]);
  });
  const live = state(d);
  edit(d, () => a.delete(0, 1));
  const collected = state(d);
  for (const n of [3, 7, 8, 9, 63, 64, 65, 100]) {
    for (const pattern of ["alternating", "gc first half", "items every third"]) {
      const ins = Array.from({ length: n }, (_, i) => {
        if (pattern === "alternating") return i % 2 ? collected : live;
        if (pattern === "gc first half") return i < n / 2 ? collected : live;
        return i % 3 === 0 ? live : collected;
      });
      scenario(`${n} readers tied, ${pattern}`, ins);
    }
  }
}

// The same, with every input starting at the tied clock (diffs from clock 1),
// so the readers meet the tie when they are sorted.
{
  const d = newDoc(1);
  const a = d.getArray("a");
  edit(d, () => {
    const inner = new Y.Array();
    a.insert(0, [inner]);
    inner.insert(0, [1, 2, 3]);
  });
  const from1 = svOf([[1, 1]]);
  const live = state(d, from1);
  edit(d, () => a.delete(0, 1));
  const collected = state(d, from1);
  for (const n of [2, 3, 4, 5, 6, 7, 8, 9, 16, 63, 64, 65, 100]) {
    for (const pattern of ["alternating", "gc first half", "items every third", "one gc last"]) {
      const ins = Array.from({ length: n }, (_, i) => {
        if (pattern === "alternating") return i % 2 ? collected : live;
        if (pattern === "gc first half") return i < n / 2 ? collected : live;
        if (pattern === "one gc last") return i === n - 1 ? collected : live;
        return i % 3 === 0 ? live : collected;
      });
      scenario(`${n} readers tied from the start, ${pattern}`, ins);
    }
  }
}

// An input with Skips merged with another: the merge's readers drop them.
{
  const skips = new Uint8Array([1, 4, 1, 0, 0, 2, 10, 3, 0, 1, 10, 4, 0]);
  const gcRuns = (client, clock, ...lens) => new Uint8Array([1, lens.length, client, clock, ...lens.flatMap((n) => [0, n]), 0]);
  scenario("merge: an input with Skips", [raw(skips), raw(gcRuns(1, 3, 2))]);
  scenario("merge: an input with Skips, second", [raw(gcRuns(1, 2, 6)), raw(skips)]);
}

// Diff edge cases from raw V1 updates: a client in two sections next to each
// other, an empty struct, Skips inside and at the end, a keyed item cut from
// an offset (yjs keeps its key bit).
{
  // [2 sections] client 1 at 0: GC 2; client 1 at 2: GC 3 | empty ds
  const adjacent = new Uint8Array([2, 1, 1, 0, 0, 2, 1, 1, 2, 0, 3, 0]);
  scenario("diff: a client in two adjacent sections", [raw(adjacent)], [svOf([[1, 1]]), svOf([[1, 3]]), svOf([[1, 5]])]);
  // client 1 at 0: GC 2, Skip 3, GC 1, Skip 4
  const skips = new Uint8Array([1, 4, 1, 0, 0, 2, 10, 3, 0, 1, 10, 4, 0]);
  scenario("diff: Skips inside and at the end", [raw(skips)], [svOf([[1, 1]]), svOf([[1, 3]]), svOf([[1, 6]])]);
  // client 1 at 0: an item with no origins, parent root "m", key "k", Any [1, 2, 3]
  const keyed = new Uint8Array([1, 1, 1, 0, 0x28, 1, 1, 0x6d, 1, 0x6b, 3, 0x7d, 1, 0x7d, 2, 0x7d, 3, 0]);
  scenario("diff: a keyed item cut from an offset", [raw(keyed)], [svOf([[1, 1]]), svOf([[1, 2]])]);
}

// Delete sets as written: a zero-length range, a client in two sections,
// ranges out of order. V1 only (V2 cannot carry an empty range, and yjs
// writes ranges out of order as a negative varuint).
{
  const v1Only = (name, inputs, svs = []) => {
    const m1 = Y.mergeUpdates(inputs);
    const allSvs = [new Uint8Array([0]), ...svs];
    scenarios.push({
      name,
      inputs_v1: inputs.map(toHex),
      inputs_v2: [],
      merge_v1: toHex(m1),
      merge_v2: "",
      diffs: allSvs.map((sv) => ({ sv: toHex(sv), v1: toHex(Y.diffUpdate(m1, sv)), v2: "" })),
      input_diffs: inputs.map((u) => ({ sv: toHex(svs[0] || new Uint8Array([0])), v1: toHex(Y.diffUpdate(u, svs[0] || new Uint8Array([0]))), v2: "" })),
    });
  };
  // GC 1:0+4; ds: client 1 [(3,0)]
  const zeroLen = new Uint8Array([1, 1, 1, 0, 0, 4, 1, 1, 1, 3, 0]);
  // GC 1:0+8; ds: client 1 [(5,1),(1,1)], then client 1 again [(3,0)]
  const outOfOrder = new Uint8Array([1, 1, 1, 0, 0, 8, 2, 1, 2, 5, 1, 1, 1, 1, 1, 3, 0]);
  // GC 2:0+2; ds: client 2 [(0,1)], client 1 [(6,2)]
  const other = new Uint8Array([1, 1, 2, 0, 0, 2, 2, 2, 1, 0, 1, 1, 1, 6, 2]);
  v1Only("delete sets: a zero-length range", [zeroLen]);
  v1Only("delete sets: a zero-length range merged", [zeroLen, other], [svOf([[1, 2]])]);
  v1Only("delete sets: a client twice, out of order", [outOfOrder], [svOf([[1, 3]])]);
  v1Only("delete sets: out of order, merged", [outOfOrder, other, zeroLen]);
}

// Text with characters outside the BMP, cut by a diff between them.
{
  const d = newDoc(1);
  const t = d.getText("t");
  const ups = [edit(d, () => t.insert(0, "a😀b")), edit(d, () => t.insert(4, "😀c"))];
  scenario("surrogate pairs", ups, [svOf([[1, 1]]), svOf([[1, 3]]), svOf([[1, 4]])]);
  scenario("surrogate pairs cut inside a pair", ups, [svOf([[1, 2]]), svOf([[1, 5]])]);
}

// A format value that is undefined exists only in V2.
{
  const d = newDoc(1);
  const t = d.getText("t");
  const u1 = edit(d, () => t.insert(0, "abc"));
  const sv = Y.encodeStateVector(d);
  d.transact(() => t.format(0, 2, { bold: undefined }));
  const u2v2 = Y.encodeStateAsUpdateV2(d, sv);
  scenarios.push({
    name: "V2 only: an undefined format value",
    inputs_v1: [],
    inputs_v2: [toHex(u1.v2), toHex(u2v2)],
    merge_v1: "",
    merge_v2: toHex(Y.mergeUpdatesV2([u1.v2, u2v2])),
    diffs: [new Uint8Array([0]), svOf([[1, 2]])].map((sv) => ({ sv: toHex(sv), v1: "", v2: toHex(Y.diffUpdateV2(Y.mergeUpdatesV2([u1.v2, u2v2]), sv)) })),
    input_diffs: [],
  });
}

writeFileSync(outPath, JSON.stringify({ generator: "gen-merge-updates.mjs (yjs 13.6.33)", scenarios }, null, 1) + "\n");
console.log(`wrote ${scenarios.length} scenarios to ${outPath}`);
