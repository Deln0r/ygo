// Generates testdata/json-text-fixtures.json: the JSON text yjs 13.6.33
// writes for V1 embeds and format values (JSON.stringify), and the lib0
// Any objects it writes in V2 and for map values, for the cases where
// encoding/json writes other text: & < > U+2028 U+2029 in strings, NaN and
// Infinity next to other values, numbers in exponent form, array-index keys
// (JavaScript enumerates them first), and a Uint8Array embed.
//
// Each scenario records yjs's V1 and V2 state update and the value it
// stored, tagged (JSON alone cannot carry NaN, Infinity or bytes). The Go
// test (json_text_fixture_test.go) re-encodes yjs's documents and requires
// yjs's bytes, then writes the same values through its API and requires
// them again. The non-index keys of every object are in sorted order: ygo
// does not keep a decoded object's key order yet.
//
// "texts" pairs JSON text with JSON.stringify(JSON.parse(text)), the text
// a value given as JSON (a json.RawMessage) must come out as, and the V1 and
// V2 state of a document holding JSON.parse of that text as an embed.
//
// Run: node gen-json-text.mjs (after npm install in this directory).

import { writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";
import * as Y from "yjs";

const here = dirname(fileURLToPath(import.meta.url));
const outPath = resolve(here, "..", "json-text-fixtures.json");
const toHex = (u8) => Array.from(u8, (b) => b.toString(16).padStart(2, "0")).join("");

function tag(v) {
  if (v === null) return null;
  if (v instanceof Uint8Array) return { $: "bytes", v: Array.from(v) };
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

const scenarios = [];
function record(name, kind, key, value, { replayV2 = true } = {}) {
  const doc = new Y.Doc();
  doc.clientID = 1;
  doc.transact(() => {
    if (kind === "map") {
      doc.getMap("m").set(key, value);
      return;
    }
    const t = doc.getText("t");
    if (kind === "embed") {
      t.insertEmbed(0, value);
    } else {
      t.insert(0, "ab");
      t.format(0, 2, { [key]: value });
    }
  });
  scenarios.push({
    name, kind, key, value: tag(value), replayV2,
    v1: toHex(Y.encodeStateAsUpdate(doc)),
    v2: toHex(Y.encodeStateAsUpdateV2(doc)),
  });
}

record("embed: & < > and line separators", "embed", "", { s: "a&b<c>d e f" });
record("embed: control characters, quote, backslash, slash, emoji, DEL", "embed", "", { s: "\u0001\u001f\"\\/\t\n\u{1F600}\u007f" });
record("format: a link with a query string", "format", "link", "https://x.test/?a=1&b=<2>");
record("format: an object value with &", "format", "meta", { href: "a&b", n: 1 });
record("embed: numbers in exponent and plain form", "embed", "", { a: 1e21, b: 1e-7, c: 0.1, d: 2.5, e: 123456789012345680000, f: 0.000001, g: -1.5e-10 });
// Go's math.NaN() has other bits than JavaScript's NaN, so V2 (which keeps
// the bits) is compared only for yjs's own document.
record("embed: NaN and Infinity next to other values", "embed", "", { i: Infinity, keep: 1, n: NaN }, { replayV2: false });
record("embed: an array with NaN and -Infinity", "embed", "", [NaN, 1, -Infinity], { replayV2: false });
record("embed: array-index keys", "embed", "", { a: 1, 10: 2, 9: 3 });
record("map value: array-index keys, nested", "map", "k", { a: 1, 10: 2, 9: { b: 1, 2: 0 } });
record("embed: a Uint8Array", "embed", "", new Uint8Array([1, 2, 255]));
// Deeper than ygo's Any tree walk (1000 levels): it goes through JSON text.
let deep = [];
for (let i = 1; i < 1500; i++) deep = [deep];
record("embed: arrays nested 1500 deep", "embed", "", { v: deep });

const texts = [
  '{"b":1,"10":2,"a":3,"9":4}',
  '{"a":1,"a":2}',
  '{"x":{"y":1,"x":2},"x":{"q":1,"p":2}}',
  '"\\u0026\\u003c\\u003e\\u2028\\u2029&<>"',
  '"\\ud800x\\udc00"',
  '"\\ud83d\\ude00"',
  '[1.0, 1e2, 1E-7, 1e400, -0, 0.1, 123456789012345680000, 1e21, 0.000001, 5e-324, -1.5e-10]',
  '"\\u0001\\u001f\\b\\t\\n\\f\\r\\"\\\\\\/\\u007f"',
  '{"4294967294":1,"4294967295":2,"01":3,"-1":4,"1":5}',
  '{"__proto__":1,"a":2}',
  ' { "a" : [ 1 , 2 ] } ',
  '{"\\ud800":1,"\\ud801":2}',
  '{"a":1,"b":2,"a":3}',
  '{"a":1,"4294967295":2}',
  '{"a":1,"4294967294":2}',
  "[".repeat(1500) + "]".repeat(1500),
].map((text) => {
  // Wrapped in an object: yjs inserts a string given to insertEmbed as text
  // (YText insertText takes the string branch), not as an embed.
  const raw = `{"v":${text}}`;
  // ygo stores JSON text in the form JSON.stringify writes (so -0 is 0 and
  // a number beyond the float64 range null), and V1 and V2 receivers both
  // get JSON.parse of that text.
  const doc = new Y.Doc();
  doc.clientID = 1;
  doc.getText("t").insertEmbed(0, JSON.parse(JSON.stringify(JSON.parse(raw))));
  return {
    raw, want: JSON.stringify(JSON.parse(raw)),
    v1: toHex(Y.encodeStateAsUpdate(doc)),
    v2: toHex(Y.encodeStateAsUpdateV2(doc)),
  };
});

writeFileSync(outPath, JSON.stringify({ generator: "gen-json-text.mjs (yjs 13.6.33)", scenarios, texts }, null, 2) + "\n");
console.log(`wrote ${scenarios.length} scenarios and ${texts.length} texts to ${outPath}`);
