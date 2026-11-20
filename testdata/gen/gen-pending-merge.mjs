// Generates testdata/pending-merge-fixtures.json: a receiver queues
// updates that wait for what they depend on and that cover the same
// clocks in different ways (one block longer than another, the same
// clocks as an item and as GC, GC runs that touch or overlap), and yjs
// 13.6.33 writes its state, whole and as diffs against several state
// vectors, V1 and V2; then the missing update arrives and the final state
// and the update events of that transaction are recorded.
//
// yjs keeps its queue as one update and merges every leftover into it
// with mergeUpdatesV2([queue, leftover]) (utils/encoding.js readUpdateV2);
// its state is mergeUpdatesV2([store part, pending deletes, the queue
// diffed against the state vector]) (encodeStateAsUpdateV2). At an equal
// start the decoder ahead in the array wins and the other is cut; which
// one is ahead is carried from client to client; GC and an item at the
// same start go to the second decoder; GC runs merge, items never. The Go
// test (pending_merge_fixture_test.go) applies the same updates, as V1
// and as V2, and requires the same bytes.
//
// Run: node gen-pending-merge.mjs (after npm install in this directory).

import { writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";
import * as Y from "yjs";

const here = dirname(fileURLToPath(import.meta.url));
const outPath = process.env.OUT || resolve(here, "..", "pending-merge-fixtures.json");
const toHex = (u8) => Array.from(u8, (b) => b.toString(16).padStart(2, "0")).join("");
const svOf = (entries) => Y.encodeStateVector(new Map(entries));
const merge = (us) => Y.mergeUpdates(us);

// edit runs fn on doc and returns the update it made.
function edit(doc, fn) {
  const sv = Y.encodeStateVector(doc);
  fn();
  return Y.encodeStateAsUpdate(doc, sv);
}
// gcRuns is a V1 update of client's GC runs, consecutive from clock
// (every number below 128, so each varuint is one byte).
const gcRuns = (client, clock, ...lens) => new Uint8Array([1, lens.length, client, clock, ...lens.flatMap((n) => [0, n]), 0]);

const scenarios = [];
function scenario(name, updates, svs, final) {
  for (const gc of [true, false]) {
    const d = new Y.Doc({ gc });
    d.clientID = 99;
    d.getText("t");
    d.getArray("a");
    for (const u of updates) Y.applyUpdate(d, u);
    if (!d.store.pendingStructs && !d.store.pendingDs) throw new Error(`${name}: nothing pending`);
    const s = {
      name: `${name} (gc ${gc ? "on" : "off"})`,
      gc,
      updates: updates.map(toHex),
      v1: toHex(Y.encodeStateAsUpdate(d)),
      v2: toHex(Y.encodeStateAsUpdateV2(d)),
      diffs: svs.map((sv) => ({ sv: toHex(sv), v1: toHex(Y.encodeStateAsUpdate(d, sv)), v2: toHex(Y.encodeStateAsUpdateV2(d, sv)) })),
    };
    if (final) {
      const events = [];
      const eventsV2 = [];
      d.on("update", (u) => events.push(toHex(u)));
      d.on("updateV2", (u) => eventsV2.push(toHex(u)));
      Y.applyUpdate(d, final);
      Object.assign(s, {
        final: toHex(final),
        finalV1: toHex(Y.encodeStateAsUpdate(d)),
        finalV2: toHex(Y.encodeStateAsUpdateV2(d)),
        finalPending: !!(d.store.pendingStructs || d.store.pendingDs),
        events,
        eventsV2,
      });
    }
    scenarios.push(s);
  }
}

// Client 1 types "h", then "abcde", then "f", "gh": u1 carries clocks 1-5
// as one item, u2 clocks 1-6, u3 clocks 1-8; all wait on clock 0.
const a = new Y.Doc();
a.clientID = 1;
const t = a.getText("t");
const u0 = edit(a, () => t.insert(0, "h"));
const u1 = edit(a, () => t.insert(1, "abcde"));
t.insert(6, "f");
const u2 = Y.encodeStateAsUpdate(a, svOf([[1, 1]]));
t.insert(7, "gh");
const u3 = Y.encodeStateAsUpdate(a, svOf([[1, 1]]));
// u1d: the same clocks 1-5 deleted by a peer, as ContentDeleted, with the
// delete; u2d: clocks 1-6 deleted.
const ad = new Y.Doc();
ad.clientID = 7;
Y.applyUpdate(ad, merge([u0, u1]));
ad.getText("t").delete(1, 5);
const u1d = Y.encodeStateAsUpdate(ad, svOf([[1, 1]]));
// Client 9 has a block waiting on its own clock 0.
const b = new Y.Doc();
b.clientID = 9;
const b0 = edit(b, () => b.getText("t").insert(0, "x"));
const b1 = edit(b, () => b.getText("t").insert(1, "y"));
const svs = [svOf([]), svOf([[1, 1]]), svOf([[1, 3]]), svOf([[1, 6]]), svOf([[1, 7]]), svOf([[9, 1]]), svOf([[1, 2], [9, 1]])];
const missing = merge([u0, b0]);

// An equal start: the queued block wins and the later one is cut, unless
// the later update was ahead from a higher client.
scenario("a shorter block queued, a longer one arrives", [u1, u2], svs, u0);
scenario("a longer block queued, a shorter one arrives", [u2, u1], svs, u0);
scenario("the arriving update also holds a higher client", [u1, merge([b1, u2])], svs, missing);
scenario("the queued update also holds a higher client", [merge([b1, u1]), u2], svs, missing);
scenario("both updates hold the higher client", [merge([b1, u1]), merge([b1, u2])], svs, missing);
scenario("three blocks at one start", [u1, u3, u2], svs, u0);
scenario("three blocks at one start, the longest first", [u3, u1, u2], svs, u0);
// The queue after a retry is what the retry left, then a longer block.
scenario("a longer block after a retry left the queue", [merge([b1, u1]), b0, u2], svs, u0);
// The same clocks live and deleted (ContentDeleted plus the delete).
scenario("a live item queued, the same clocks deleted arrive", [u1, u1d], svs, u0);
scenario("the same clocks deleted queued, a live item arrives", [u1d, u1], svs, u0);
scenario("a live item queued, the deleted clocks arrive with a higher client", [u1, merge([b1, u1d])], svs, missing);
{
  const ad2 = new Y.Doc();
  ad2.clientID = 7;
  Y.applyUpdate(ad2, merge([u0, u2]));
  ad2.getText("t").delete(1, 6);
  scenario("a shorter live item queued, a longer deleted one arrives", [u1, Y.encodeStateAsUpdate(ad2, svOf([[1, 1]]))], svs, u0);
}

// A cut through a surrogate pair: the queued block ends where the
// arriving one has the first half of a pair.
{
  const p = new Y.Doc();
  Y.applyUpdate(p, u0);
  p.clientID = 1; // after the apply: yjs draws a new ID when its own clock moves
  const q = edit(p, () => p.getText("t").insert(1, "ab\uD83D")); // the lone half is written as U+FFFD
  const r = new Y.Doc();
  Y.applyUpdate(r, u0);
  r.clientID = 1;
  const full = edit(r, () => r.getText("t").insert(1, "ab\u{1F600}x"));
  scenario("a cut through a surrogate pair", [q, full], [svOf([]), svOf([[1, 3]]), svOf([[1, 4]]), svOf([[1, 5]])], u0);
}

// Any items: the arriving array run is cut after the queued one.
{
  const p = new Y.Doc();
  p.clientID = 1;
  const arr = p.getArray("a");
  const v0 = edit(p, () => arr.insert(0, ["h"]));
  const v1 = edit(p, () => arr.insert(1, ["x", { k: 1 }]));
  arr.insert(3, [[2, 3], null]);
  const v2 = Y.encodeStateAsUpdate(p, svOf([[1, 1]]));
  scenario("array values queued, a longer run arrives", [v1, v2], [svOf([]), svOf([[1, 2]]), svOf([[1, 3]]), svOf([[1, 4]])], v0);
}

// GC runs merge across the two updates, items never.
scenario("adjacent GC runs from two updates", [gcRuns(1, 2, 2), gcRuns(1, 4, 2)], [svOf([]), svOf([[1, 3]]), svOf([[1, 4]]), svOf([[1, 5]])], gcRuns(1, 0, 2));
scenario("overlapping GC runs from two updates", [gcRuns(1, 2, 3), gcRuns(1, 4, 3)], [svOf([]), svOf([[1, 4]]), svOf([[1, 5]])], gcRuns(1, 0, 2));
scenario("a GC run covered by another", [gcRuns(1, 2, 5), gcRuns(1, 3, 2)], [svOf([]), svOf([[1, 4]])], gcRuns(1, 0, 2));
scenario("two adjacent GC runs in one update", [gcRuns(1, 2, 2, 2)], [svOf([]), svOf([[1, 4]])], gcRuns(1, 0, 2));
scenario("a GC run, then two adjacent ones touching it", [gcRuns(1, 2, 2), gcRuns(1, 4, 2, 2)], [svOf([]), svOf([[1, 5]]), svOf([[1, 6]])], gcRuns(1, 0, 2));
scenario("GC runs with a gap between them", [gcRuns(1, 2, 2), gcRuns(1, 5, 2)], [svOf([]), svOf([[1, 4]]), svOf([[1, 5]])], gcRuns(1, 0, 2));

// An empty GC run of a higher client (a malformed update yjs accepts): its
// reader keeps it, so the leftover is in front from client 2 on, and at
// client 1 the item queued before wins the tie with the GC run.
{
  const fromHex = (h) => Uint8Array.from(Buffer.from(h, "hex"));
  const item = fromHex("01010101840100014100"); // client 1, clock 1, "A" after 1:0
  const emptyThenGC = fromHex("020102010000010101000100"); // client 2: GC of length 0; client 1: GC 1:1
  scenario("an empty GC run of a higher client", [item, emptyThenGC], [svOf([]), svOf([[1, 1]]), svOf([[2, 1]])], u0);
}

// An empty struct at the start of a waiting client: yjs writes its
// leftover from findIndexSS(structs, the first clock), which passes over
// it when a later struct covers that clock (here the GC run after it, or a
// Skip), so the empty item never reaches the queue.
{
  const fromHex = (h) => Uint8Array.from(Buffer.from(h, "hex"));
  const h = fromHex("0101010004010174016800"); // client 1, clock 0: "h"
  const emptyThenGC = fromHex("010201010401017400000100"); // client 1, clock 1: empty item, GC 1
  const item = fromHex("01010101840100014100"); // client 1, clock 1: "A" after 1:0
  scenario("an empty item before a GC run at one clock", [emptyThenGC, item], [svOf([]), svOf([[1, 1]]), svOf([[1, 2]])], h);
  const emptySkipItem = fromHex("0103010104010174000a02840100014200"); // client 1, clock 1: empty item, Skip 2, "B" after 1:0
  scenario("an empty item before a Skip", [emptySkipItem], [svOf([]), svOf([[1, 1]]), svOf([[1, 3]])], h);
}

// The same clocks as an item and as GC: the second decoder wins.
{
  const it = Y.decodeUpdate(u1).structs[0];
  const g = gcRuns(1, it.id.clock, it.length);
  scenario("an item queued, GC for the same clocks arrives", [u1, g], svs, u0);
  scenario("GC queued, an item for the same clocks arrives", [g, u1], svs, u0);
  scenario("an item queued, GC for the same clocks arrives with a higher client", [u1, merge([b1, g])], svs, missing);
  scenario("GC queued with a higher client, an item for the same clocks arrives", [merge([b1, g]), u1], svs, missing);
}

// The store has client 1's clocks 0-1 as GC; client 1's item 0-2 waits on
// client 5. In the state the GC and the item start at the same clock.
// (yjs itself throws integrating client 5's update afterwards, so there
// is no final step.)
{
  const c = new Y.Doc();
  c.clientID = 5;
  const c0 = edit(c, () => c.getText("t").insert(0, "z"));
  const e = new Y.Doc();
  e.clientID = 1;
  Y.applyUpdate(e, c0);
  edit(e, () => e.getText("t").insert(1, "abc"));
  const eu = Y.encodeStateAsUpdate(e, svOf([[5, 1]]));
  const ssvs = [svOf([]), svOf([[1, 1]]), svOf([[1, 2]]), svOf([[9, 1]])];
  scenario("store GC and a waiting item at the same clock", [gcRuns(1, 0, 2), eu], ssvs, null);
  scenario("store GC and a waiting item at the same clock, a higher client waiting too", [gcRuns(1, 0, 2), merge([b1, eu])], ssvs, null);
}

// The state re-reads the store part: an item with an origin comes back
// without its key (yjs's reader takes the parent and the key only from an
// item without origins), so a map value that overwrote another loses the
// key bit once anything waits; a keyed run cut at the state vector keeps
// it in a plain diff and loses it in the merged one.
{
  const m = new Y.Doc();
  m.clientID = 1;
  const mm = m.getMap("m");
  const k0 = edit(m, () => mm.set("k", 1));
  const k1 = edit(m, () => mm.set("k", 2));
  const k2 = edit(m, () => {
    mm.set("k", 3);
    mm.set("j", "x");
  });
  const full = merge([k0, k1, k2]);
  const msvs = [svOf([]), svOf([[1, 1]]), svOf([[1, 2]]), svOf([[1, 3]])];
  scenario("map values overwritten, a GC run waits", [full, gcRuns(3, 2, 2)], msvs, gcRuns(3, 0, 2));
  const n = new Y.Doc();
  n.clientID = 4;
  const n0 = edit(n, () => n.getArray("a").insert(0, ["p", "q"]));
  const n1 = edit(n, () => n.getArray("a").delete(0, 1));
  scenario("map values overwritten, a delete waits", [full, n1], msvs, n0);
}

// A keyed run waits (a key's earlier values, deleted, one run without an
// origin), and the state vector cuts it: written from an offset it has an
// origin, and yjs reads it back without the key.
{
  const m2 = new Y.Doc();
  m2.clientID = 1;
  const h0 = edit(m2, () => m2.getText("t").insert(0, "h"));
  const mm2 = m2.getMap("m");
  edit(m2, () => mm2.set("k", 1));
  edit(m2, () => mm2.set("k", 2));
  edit(m2, () => mm2.set("k", 3));
  const waiting = Y.encodeStateAsUpdate(m2, svOf([[1, 1]]));
  scenario("a keyed deleted run waits, cut by the state vector", [waiting], [svOf([]), svOf([[1, 1]]), svOf([[1, 2]]), svOf([[1, 3]])], h0);
}

// A nested text's first item (a parent by ID, no origin) queued; GC for
// the first two of its clocks arrives and wins the tie; the cut tail has
// an origin, so yjs reads it back without the parent and integrates it
// once client 1's clock 0 is there, though the parent never came.
{
  const p = new Y.Doc();
  p.clientID = 5;
  const inner = new Y.Text();
  const p0 = edit(p, () => p.getArray("a").insert(0, [inner]));
  const q = new Y.Doc();
  Y.applyUpdate(q, p0);
  q.clientID = 1;
  const h = edit(q, () => q.getText("t").insert(0, "h"));
  const nested = edit(q, () => q.getArray("a").get(0).insert(0, "abc"));
  const it = Y.decodeUpdate(nested).structs.find((s) => s.id.client === 1);
  scenario("a nested first item queued, GC for its first clocks arrives", [nested, gcRuns(1, it.id.clock, 2)], [svOf([]), svOf([[1, 2]]), svOf([[1, 3]])], h);
}

// A retry that leaves part of the queue, with deletes of the update and
// deletes already queued.
scenario("a retry leaves part of the queue, fresh and queued deletes", [u1d, merge([b1, u2]), merge([b0, u1d])], svs, u0);

for (const s of scenarios) {
  s.updatesV2 = s.updates.map((u) => toHex(Y.convertUpdateFormatV1ToV2(Uint8Array.from(Buffer.from(u, "hex")))));
  if (s.final) s.finalUpdateV2 = toHex(Y.convertUpdateFormatV1ToV2(Uint8Array.from(Buffer.from(s.final, "hex"))));
}
// The fixtures depend on how V8 sorts two decoders whose comparator says
// each goes before the other (GC and an item at one clock): it compares
// the second with the first once and swaps them. Refuse to write them
// from a runtime that does otherwise.
{
  const pair = ["first", "second"];
  let calls = 0;
  pair.sort(() => {
    calls++;
    return -1;
  });
  if (pair[0] !== "second" || calls !== 1) throw new Error(`this runtime sorts two elements differently (${pair}, ${calls} calls)`);
}
// (Not written into the file: CI regenerates it with its own Node and
// requires no diff. Checked with Node 26.6.0, V8 14.6.)
writeFileSync(outPath, JSON.stringify({ generator: "gen-pending-merge.mjs (yjs 13.6.33)", scenarios }, null, 2) + "\n");
console.log(`wrote ${scenarios.length} scenarios to ${outPath}`);
