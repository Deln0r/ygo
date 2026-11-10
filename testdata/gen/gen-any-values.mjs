// Generates testdata/any-value-fixtures.json: documents from yjs 13.6.33
// holding the lib0 Any values ygo changed before v1.33.0, undefined, BigInt
// and -0, as map values, nested in arrays and objects, in a subdocument's
// meta, and (V2 only, since V1 writes JSON) in format attributes and embeds.
//
// Each scenario records yjs's V1 and V2 state update (V1 is null where yjs
// cannot write it: JSON.stringify throws on a BigInt) and the value a reader
// gets, in a tagged form: JSON alone cannot tell undefined from null, drops
// the sign of zero and has no BigInt.
//
// The Go test (any_value_fixture_test.go) applies each update, checks the Go
// values, re-encodes the document and requires yjs's bytes exactly, and for
// the scenarios it can replay through its own API, writes the same values
// and requires yjs's bytes again.
//
// Objects use keys already in sorted order: ygo sorts object keys when it
// encodes (README), so only sorted objects can be compared byte for byte.
//
// Run: node gen-any-values.mjs (after npm install in this directory).

import { writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";
import * as Y from "yjs";

const here = dirname(fileURLToPath(import.meta.url));
const outPath = resolve(here, "..", "any-value-fixtures.json");

const toHex = (u8) => Array.from(u8, (b) => b.toString(16).padStart(2, "0")).join("");

// tag encodes a JS value so that undefined, -0 and BigInt survive JSON.
function tag(v) {
  if (v === undefined) return { $: "undefined" };
  if (v === null) return null;
  if (typeof v === "bigint") return { $: "bigint", v: v.toString() };
  if (typeof v === "number") return { $: "number", v: Object.is(v, -0) ? "-0" : String(v) };
  if (typeof v === "string" || typeof v === "boolean") return v;
  if (Array.isArray(v)) return { $: "array", v: v.map(tag) };
  if (typeof v === "object") {
    const o = {};
    for (const k of Object.keys(v)) o[k] = tag(v[k]);
    return { $: "object", v: o };
  }
  throw new Error(`cannot tag ${typeof v}`);
}

const values = {
  "undefined": undefined,
  "null": null,
  "-0": -0,
  "0": 0,
  "2.5": 2.5,
  "BigInt 5": 5n,
  "BigInt -1": -1n,
  "BigInt 2^53+1": 2n ** 53n + 1n,
  "BigInt min int64": -(2n ** 63n),
  "object with undefined, -0, BigInt": { a: undefined, b: -0, c: 5n },
  "array with undefined, -0, BigInt, null": [undefined, -0, 5n, null],
  "nested undefined": { a: [{ b: undefined }] },
};

const scenarios = [];

function record(name, kind, build, read, { v1 = true } = {}) {
  const doc = new Y.Doc();
  doc.clientID = 1;
  doc.transact(() => build(doc));
  const v2 = Y.encodeStateAsUpdateV2(doc);
  let v1hex = null;
  if (v1) v1hex = toHex(Y.encodeStateAsUpdate(doc));
  // Read back from a fresh document, as a peer would.
  const fresh = new Y.Doc();
  Y.applyUpdateV2(fresh, v2);
  scenarios.push({ name, kind, v1: v1hex, v2: toHex(v2), expect: tag(read(fresh)) });
}

for (const [name, value] of Object.entries(values)) {
  record(`map value: ${name}`, "map", (d) => d.getMap("m").set("k", value), (d) => {
    if (!d.getMap("m").has("k")) throw new Error("key lost");
    return d.getMap("m").get("k");
  });
}

for (const [name, value] of Object.entries({
  "object with undefined": { a: undefined },
  "array with undefined, -0, BigInt": [undefined, -0, 5n],
})) {
  record(`array element: ${name}`, "array", (d) => d.getArray("a").insert(0, [value]), (d) => d.getArray("a").get(0));
}

record("subdocument meta with undefined, BigInt, -0", "subdoc",
  (d) => d.getMap("m").set("d", new Y.Doc({ guid: "subdoc-guid", meta: { a: undefined, b: 5n, c: -0 } })),
  (d) => d.getMap("m").get("d").meta);

for (const [name, value] of Object.entries({
  "BigInt 5": 5n,
  "BigInt 2^53+1": 2n ** 53n + 1n,
  "-0": -0,
  "object with undefined and BigInt": { a: undefined, b: 5n },
})) {
  const v1 = typeof value !== "bigint" && !(typeof value === "object" && Object.values(value).some((x) => typeof x === "bigint"));
  record(`format attribute: ${name}`, "format", (d) => {
    const t = d.getText("t");
    t.insert(0, "ab");
    t.format(0, 2, { x: value });
  }, (d) => d.getText("t").toDelta()[0].attributes.x, { v1 });
}

record("embed with undefined, -0, BigInt", "embed",
  (d) => d.getText("t").insertEmbed(0, { a: undefined, b: -0, c: 5n }),
  (d) => d.getText("t").toDelta()[0].insert, { v1: false });

writeFileSync(outPath, JSON.stringify({ generator: "gen-any-values.mjs (yjs 13.6.33)", scenarios }, null, 2) + "\n");
console.log(`wrote ${scenarios.length} scenarios to ${outPath}`);
