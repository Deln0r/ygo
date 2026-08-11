// Generates testdata/wire-edge-fixtures.json — V1 full-update wire-byte
// fixtures from JS Yjs (yjs@13.6.32) that exercise two byte-exactness
// edge cases the single-client fixtures never covered:
//
//   1. multi-client documents with deletes, which require the delete
//      set AND the client-struct runs to be emitted in DESCENDING
//      client order to match yjs byte-for-byte.
//   2. wide client IDs (> 2^32), which yjs@14 will produce as it widens
//      clientID from 32 to 53 bits. ygo already uses uint64 + varint
//      throughout; this locks that in against regression and proves
//      forward-compatibility with the v14 clientID space.
//
// The Go test (wire_edge_fixture_test.go) applies each update to a
// fresh doc and re-encodes it byte-identically.
//
// Run: node gen-wire-edge.mjs (after npm install in this directory).

import { writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";
import * as Y from "yjs";

const here = dirname(fileURLToPath(import.meta.url));
const outPath = resolve(here, "..", "wire-edge-fixtures.json");

const toHex = (u8) =>
  Array.from(u8, (b) => b.toString(16).padStart(2, "0")).join("");

const scenarios = [];

// 1. Multi-client array with deletes from both clients (descending DS).
scenarios.push({
  description: "multi-client array with deletes",
  root_name: "a",
  root_kind: "array",
  expected: ["A", "D"],
  build() {
    const d1 = new Y.Doc();
    d1.clientID = 5;
    const a1 = d1.getArray("a");
    a1.insert(0, ["A", "B", "C"]);
    a1.delete(1, 1); // delete B (client 5)

    const d2 = new Y.Doc();
    d2.clientID = 200;
    Y.applyUpdate(d2, Y.encodeStateAsUpdate(d1));
    const a2 = d2.getArray("a");
    a2.insert(2, ["D", "E"]); // append after [A, C]
    a2.delete(2, 1); // delete C is index 1 now... delete the just-added? keep simple: delete index 2 (E)

    Y.applyUpdate(d1, Y.encodeStateAsUpdate(d2));
    return d1;
  },
});

// 2. Wide client ID (> 2^32). 2^40 + 12345 = 1099511640889.
scenarios.push({
  description: "wide client id (> 2^32)",
  root_name: "m",
  root_kind: "map",
  expected_map: { k: "v", n: 7 },
  build() {
    const d = new Y.Doc();
    d.clientID = 1099511640889;
    const m = d.getMap("m");
    m.set("k", "v");
    m.set("n", 7);
    return d;
  },
});

// 3. Two wide client IDs merged (descending order with large values).
scenarios.push({
  description: "two wide client ids merged",
  root_name: "a",
  root_kind: "array",
  expected: ["x", "y"],
  build() {
    const d1 = new Y.Doc();
    d1.clientID = 5000000000; // ~2^32.2
    d1.getArray("a").insert(0, ["x"]);

    const d2 = new Y.Doc();
    d2.clientID = 9000000000;
    Y.applyUpdate(d2, Y.encodeStateAsUpdate(d1));
    d2.getArray("a").insert(1, ["y"]);

    Y.applyUpdate(d1, Y.encodeStateAsUpdate(d2));
    return d1;
  },
});

const out = {
  generator: "yjs@13.6.32 (wire edge V1)",
  scenarios: scenarios.map((s) => {
    const doc = s.build();
    const row = {
      description: s.description,
      root_name: s.root_name,
      root_kind: s.root_kind,
      update_hex: toHex(Y.encodeStateAsUpdate(doc)),
    };
    // Record the real post-build state from yjs so the Go test checks
    // against ground truth, not a hand-written guess.
    if (s.root_kind === "array") {
      row.expected = doc.getArray(s.root_name).toArray();
    } else if (s.root_kind === "map") {
      row.expected_map = doc.getMap(s.root_name).toJSON();
    }
    return row;
  }),
};

writeFileSync(outPath, JSON.stringify(out, null, 2) + "\n");
console.log(`wrote ${out.scenarios.length} wire-edge scenarios to ${outPath}`);
