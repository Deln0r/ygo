// Generates testdata/pending-state-fixtures.json: a receiver applies a
// peer's updates in an order that leaves some of them waiting for what
// they depend on (yjs's pending structs and pending delete set), and yjs
// 13.6.33 writes its state, whole and as diffs against several state
// vectors, V1 and V2. yjs's encodeStateAsUpdate appends the waiting
// updates to what it has integrated (utils/encoding.js
// encodeStateAsUpdateV2), so a replica relays what it holds but cannot
// integrate yet. The Go test (pending_state_fixture_test.go) applies the
// same updates in the same order and requires the same bytes.
//
// Run: node gen-pending-state.mjs (after npm install in this directory).

import { writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";
import * as Y from "yjs";

const here = dirname(fileURLToPath(import.meta.url));
const outPath = resolve(here, "..", "pending-state-fixtures.json");
const toHex = (u8) => Array.from(u8, (b) => b.toString(16).padStart(2, "0")).join("");

// edits runs fn on doc and returns the update it made.
function edit(doc, fn) {
  const sv = Y.encodeStateVector(doc);
  fn();
  return Y.encodeStateAsUpdate(doc, sv);
}
const sync = (from, to) => Y.applyUpdate(to, Y.encodeStateAsUpdate(from, Y.encodeStateVector(to)));
const svOf = (entries) => Y.encodeStateVector(new Map(entries));

const scenarios = [];
function scenario(name, updates, svs) {
  for (const gc of [true, false]) {
    const d = new Y.Doc({ gc });
    d.clientID = 99;
    d.getArray("a");
    d.getText("t");
    for (const u of updates) Y.applyUpdate(d, u);
    if (!d.store.pendingStructs && !d.store.pendingDs) throw new Error(`${name}: nothing pending`);
    scenarios.push({
      name: `${name} (gc ${gc ? "on" : "off"})`,
      gc,
      updates: updates.map(toHex),
      v1: toHex(Y.encodeStateAsUpdate(d)),
      v2: toHex(Y.encodeStateAsUpdateV2(d)),
      diffs: svs.map((sv) => ({ sv: toHex(sv), v1: toHex(Y.encodeStateAsUpdate(d, sv)), v2: toHex(Y.encodeStateAsUpdateV2(d, sv)) })),
    });
  }
}

// A peer (client 1) appends three times; the receiver gets the first and
// the third: the third waits for the second, a hole in the peer's clocks.
{
  const a = new Y.Doc();
  a.clientID = 1;
  const arr = a.getArray("a");
  const u1 = edit(a, () => arr.insert(0, ["a", "b"]));
  const u2 = edit(a, () => arr.insert(2, ["c", "c2"]));
  const u3 = edit(a, () => arr.insert(4, ["d", "e"]));
  void u2;
  // State vectors: inside the integrated item (1), at the hole (2), inside
  // the hole (3), at the waiting block (4), inside it (5), at its end (6),
  // past everything (9).
  scenario("a hole in the peer's own clocks", [u1, u3],
    [svOf([]), svOf([[1, 1]]), svOf([[1, 2]]), svOf([[1, 3]]), svOf([[1, 4]]), svOf([[1, 5]]), svOf([[1, 6]]), svOf([[1, 9]]), svOf([[99, 0], [1, 1]])]);
}

// A second peer (client 2) inserts between the first peer's items; the
// receiver gets the second peer's update alone: it waits on client 1.
{
  const a = new Y.Doc();
  a.clientID = 1;
  const b = new Y.Doc();
  b.clientID = 2;
  edit(a, () => a.getArray("a").insert(0, ["a", "b"]));
  sync(a, b);
  const ub = edit(b, () => b.getArray("a").insert(1, ["x", "y"]));
  scenario("a block that waits on another client", [ub],
    [svOf([]), svOf([[2, 1]]), svOf([[2, 2]]), svOf([[1, 2]]), svOf([[1, 2], [2, 1]])]);
}

// The receiver gets a delete of items it has never seen: a pending
// delete set, nothing else.
{
  const a = new Y.Doc();
  a.clientID = 1;
  edit(a, () => a.getArray("a").insert(0, ["a", "b", "c"]));
  const del = edit(a, () => a.getArray("a").delete(1, 1));
  scenario("a delete of items not seen yet", [del], [svOf([]), svOf([[1, 3]])]);
}

// Both: the receiver has the first update, gets a later insert that
// waits on a hole and a delete of the missing items.
{
  const a = new Y.Doc();
  a.clientID = 1;
  const arr = a.getArray("a");
  const u1 = edit(a, () => arr.insert(0, ["a"]));
  edit(a, () => arr.insert(1, ["b", "c"]));
  const u3 = edit(a, () => arr.insert(3, ["d"]));
  const u4 = edit(a, () => arr.delete(1, 2));
  scenario("a block behind a hole and a delete of the missing items", [u1, u3, u4],
    [svOf([]), svOf([[1, 1]]), svOf([[1, 3]]), svOf([[1, 4]])]);
}

// Two peers each with a block waiting: client 3 waits on client 2's
// second insert, client 2's second insert waits on its first.
{
  const a = new Y.Doc();
  a.clientID = 2;
  const b = new Y.Doc();
  b.clientID = 3;
  const t = a.getText("t");
  const u1 = edit(a, () => t.insert(0, "hello"));
  const u2 = edit(a, () => t.insert(5, " world"));
  sync(a, b);
  const u3 = edit(b, () => b.getText("t").insert(7, "!"));
  void u1;
  scenario("blocks of two clients waiting, text", [u3, u2],
    [svOf([]), svOf([[2, 5]]), svOf([[2, 11]]), svOf([[3, 1]]), svOf([[2, 5], [3, 0]])]);
}

// A pending block whose start the receiver already has: the peer sent
// its whole run, which waits on another client, after the receiver got
// the start of it alone.
{
  const c = new Y.Doc();
  c.clientID = 4;
  const a = new Y.Doc();
  a.clientID = 1;
  const arr = a.getArray("a");
  const u1 = edit(a, () => arr.insert(0, ["a", "b"]));
  sync(a, c);
  edit(c, () => c.getArray("a").insert(2, ["z"]));
  sync(c, a);
  edit(a, () => arr.insert(3, ["c", "d"]));
  const full = Y.encodeStateAsUpdate(a, svOf([[4, 1]])); // client 1's run whole, client 4's z left out
  scenario("a waiting block whose start the receiver has", [u1, full],
    [svOf([]), svOf([[1, 1]]), svOf([[1, 2]]), svOf([[1, 3]]), svOf([[4, 1]])]);
}

writeFileSync(outPath, JSON.stringify({ generator: "gen-pending-state.mjs (yjs 13.6.33)", scenarios }, null, 2) + "\n");
console.log(`wrote ${scenarios.length} scenarios to ${outPath}`);
