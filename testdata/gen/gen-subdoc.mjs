// Generates testdata/subdoc-fixtures.json — V1 update wire-byte
// fixtures for documents containing subdocuments (ContentDoc, ref 9),
// captured from JS Yjs (yjs@13.6.32).
//
// Each scenario nests one or more Y.Doc subdocuments and records the
// parent document's encodeStateAsUpdate bytes plus the expected subdoc
// GUIDs per map key. The Go test in subdoc_fixture_test.go applies the
// update, reads each subdoc GUID back via Map.GetDoc, and re-encodes
// byte-identically.
//
// Run: node gen-subdoc.mjs (after npm install in this directory).

import { writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";
import * as Y from "yjs";

const here = dirname(fileURLToPath(import.meta.url));
const outPath = resolve(here, "..", "subdoc-fixtures.json");

const toHex = (u8) =>
  Array.from(u8, (b) => b.toString(16).padStart(2, "0")).join("");

const scenarios = [];

// 1. Single subdoc under a map key.
scenarios.push({
  description: "single subdoc in map",
  root_name: "m",
  build() {
    const d = new Y.Doc();
    d.clientID = 11;
    const m = d.getMap("m");
    const sub = new Y.Doc({ guid: "sub-guid-aaaa" });
    m.set("child", sub);
    return { doc: d, expect: { child: "sub-guid-aaaa" } };
  },
});

// 2. Two subdocs under different keys.
scenarios.push({
  description: "two subdocs in map",
  root_name: "m",
  build() {
    const d = new Y.Doc();
    d.clientID = 12;
    const m = d.getMap("m");
    m.set("a", new Y.Doc({ guid: "guid-a-1111" }));
    m.set("b", new Y.Doc({ guid: "guid-b-2222" }));
    return { doc: d, expect: { a: "guid-a-1111", b: "guid-b-2222" } };
  },
});

// 3. Subdoc alongside a plain value (mixed content).
scenarios.push({
  description: "subdoc plus scalar key",
  root_name: "m",
  build() {
    const d = new Y.Doc();
    d.clientID = 13;
    const m = d.getMap("m");
    m.set("name", "parent");
    m.set("doc", new Y.Doc({ guid: "guid-mixed-33" }));
    return { doc: d, expect: { doc: "guid-mixed-33" } };
  },
});

const out = {
  generator: "yjs@13.6.32 (ContentDoc V1)",
  scenarios: scenarios.map((s) => {
    const { doc, expect } = s.build();
    return {
      description: s.description,
      root_name: s.root_name,
      update_hex: toHex(Y.encodeStateAsUpdate(doc)),
      expect_guids: expect,
    };
  }),
};

writeFileSync(outPath, JSON.stringify(out, null, 2) + "\n");
console.log(`wrote ${out.scenarios.length} subdoc scenarios to ${outPath}`);
