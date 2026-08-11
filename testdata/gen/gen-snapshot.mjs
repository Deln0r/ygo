// Generates testdata/snapshot-fixtures.json — V1 Snapshot wire-byte
// fixtures captured from JS Yjs (yjs@13.6.32).
//
// A snapshot is encodeSnapshot(Y.snapshot(doc)) = writeDeleteSet(ds)
// ++ writeStateVector(sv). The Go test in internal/encoding/
// snapshot_fixture_test.go round-trips each hex (decode then re-encode)
// and asserts byte-identical output, which proves ygo's snapshot codec
// is byte-compatible with yjs including the descending client order in
// the delete set. Multi-client scenarios specifically exercise that
// ordering.
//
// Run: node gen-snapshot.mjs (after npm install in this directory).

import { writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";
import * as Y from "yjs";

const here = dirname(fileURLToPath(import.meta.url));
const outPath = resolve(here, "..", "snapshot-fixtures.json");

const toHex = (u8) =>
  Array.from(u8, (b) => b.toString(16).padStart(2, "0")).join("");

function svToObject(sv) {
  const o = {};
  for (const [client, clock] of sv.entries()) o[String(client)] = clock;
  return o;
}

function dsToObject(ds) {
  const o = {};
  for (const [client, items] of ds.clients.entries()) {
    o[String(client)] = items.map((it) => [it.clock, it.len]);
  }
  return o;
}

const scenarios = [];

// 1. Empty doc.
scenarios.push({
  description: "empty doc",
  build() {
    const d = new Y.Doc();
    d.clientID = 1;
    d.getArray("a");
    return d;
  },
});

// 2. Single client, inserts + one delete.
scenarios.push({
  description: "single client array insert + delete",
  build() {
    const d = new Y.Doc();
    d.clientID = 7;
    const a = d.getArray("a");
    a.insert(0, ["x", "y", "z"]);
    a.delete(1, 1);
    return d;
  },
});

// 3. Single client, multiple separate deletes (multi-range DS).
scenarios.push({
  description: "single client multiple deletes",
  build() {
    const d = new Y.Doc();
    d.clientID = 42;
    const a = d.getArray("a");
    a.insert(0, ["a", "b", "c", "d", "e"]);
    a.delete(0, 1);
    a.delete(2, 1); // after first delete, indexes shift
    return d;
  },
});

// 4. Multi-client: two clients each contribute inserts and deletes,
// merged into one doc. The delete set then has two clients, which
// exercises the DESCENDING client-order requirement in writeDeleteSet.
scenarios.push({
  description: "multi-client merged with deletes (descending DS order)",
  build() {
    const d1 = new Y.Doc();
    d1.clientID = 3;
    const a1 = d1.getArray("a");
    a1.insert(0, ["A", "B", "C"]);
    a1.delete(1, 1);

    const d2 = new Y.Doc();
    d2.clientID = 99;
    // d2 must share the same root so updates merge into one type.
    Y.applyUpdate(d2, Y.encodeStateAsUpdate(d1));
    const a2 = d2.getArray("a");
    // d1 left [A, C] visible (B deleted); append at the end (index 2).
    a2.insert(2, ["D", "E"]);
    a2.delete(0, 1); // delete A

    // Merge d2's contributions back into d1.
    Y.applyUpdate(d1, Y.encodeStateAsUpdate(d2));
    return d1;
  },
});

const out = {
  generator: "yjs@13.6.32 (Snapshot V1)",
  scenarios: scenarios.map((s) => {
    const doc = s.build();
    const snap = Y.snapshot(doc);
    return {
      description: s.description,
      snapshot_hex: toHex(Y.encodeSnapshot(snap)),
      sv: svToObject(snap.sv),
      ds: dsToObject(snap.ds),
    };
  }),
};

writeFileSync(outPath, JSON.stringify(out, null, 2) + "\n");
console.log(`wrote ${out.scenarios.length} snapshot scenarios to ${outPath}`);
