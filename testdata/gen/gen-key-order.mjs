// Generates testdata/key-order-fixtures.json: documents from yjs 13.6.33
// whose values hold objects with keys out of sorted order, as JavaScript
// keeps them (insertion order, array-index keys first). yjs writes them
// back in that order whenever it encodes the document; ygo used to sort
// them.
//
// Each scenario gives the updates to apply, in order, and yjs's V1 and V2
// state of the resulting document. The Go test (key_order_fixture_test.go)
// applies the updates, V1 and V2, and requires yjs's state bytes, and
// requires them again from ygo.MergeUpdates of the updates.
//
// "foreign" scenarios patch a yjs update with object bytes no JavaScript
// writer produces (keys out of enumeration order, a repeated key), as
// another implementation could send; the expected state is what yjs
// writes after applying the patched update.
//
// Run: node gen-key-order.mjs (after npm install in this directory).

import { writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";
import * as Y from "yjs";
import * as encoding from "lib0/encoding";

const here = dirname(fileURLToPath(import.meta.url));
const outPath = resolve(here, "..", "key-order-fixtures.json");
const toHex = (u8) => Array.from(u8, (b) => b.toString(16).padStart(2, "0")).join("");

const scenarios = [];
function push(name, updates, doc) {
  scenarios.push({
    name,
    updates: updates.map(toHex),
    // The same updates as yjs converts them to V2 (it reads the objects
    // and writes them again, so foreign key orders are normalized here).
    updatesV2: updates.map((u) => toHex(Y.convertUpdateFormatV1ToV2(u))),
    v1: toHex(Y.encodeStateAsUpdate(doc)),
    v2: toHex(Y.encodeStateAsUpdateV2(doc)),
  });
}
function single(name, build) {
  const d = new Y.Doc();
  d.clientID = 1;
  d.transact(() => build(d));
  push(name, [Y.encodeStateAsUpdate(d)], d);
}

single("map value in insertion order", (d) => d.getMap("m").set("k", { b: 1, a: 2 }));
single("nested objects and arrays", (d) => d.getMap("m").set("k", { z: { y: 1, x: 2 }, a: [{ d: 1, c: 2 }, 3] }));
single("array-index keys with the others out of order", (d) => d.getMap("m").set("k", { b: 1, 10: 2, a: 3, 9: 4 }));
single("embed", (d) => d.getText("t").insertEmbed(0, { src: "s", alt: "a" }));
single("format value", (d) => {
  const t = d.getText("t");
  t.insert(0, "abcd");
  t.format(0, 4, { link: { title: "t", href: "h" } });
});
// More keys than decoding keeps inline (16), each holding an object of its
// own, so the orders inside go through the decoder's map.
{
  const big = {};
  for (let i = 19; i >= 0; i--) big["k" + String(i).padStart(2, "0")] = { y: i, x: i };
  single("object of twenty keys, each holding an object", (d) => d.getMap("m").set("k", big));
}
// Nested past the depth ygo walks a caller's value (1000): the embed and
// the format value keep their order in V2 too.
const nested = (depth) => { let v = 0; for (let i = 0; i < depth; i++) v = [v]; return v; };
single("embed nested 1001 levels", (d) => d.getText("t").insertEmbed(0, { b: 1, a: nested(1001) }));
single("format value nested 1001 levels", (d) => {
  const t = d.getText("t");
  t.insert(0, "abcd");
  t.format(0, 4, { link: { b: 1, a: nested(1001) } });
});
single("map value nested 1001 levels", (d) => d.getMap("m").set("k", { b: 1, a: nested(1001) }));

// Several objects in one item, then an insert between them splits it.
{
  const d = new Y.Doc();
  d.clientID = 1;
  const a = d.getArray("a");
  a.insert(0, [{ b: 1, a: 2 }, { d: 1, c: 2 }, { f: 1, e: 2 }]);
  const first = Y.encodeStateAsUpdate(d);
  a.insert(1, ["x"]);
  push("array item split between objects", [first, Y.encodeStateAsUpdate(d, Y.encodeStateVectorFromUpdate(first))], d);
}

// The same client appends in two transactions: ygo applies the second
// update in a transaction of its own and merges the new item into the
// first at commit.
{
  const d = new Y.Doc();
  d.clientID = 1;
  const a = d.getArray("a");
  a.insert(0, [{ b: 1, a: 2 }]);
  const first = Y.encodeStateAsUpdate(d);
  a.insert(1, [{ d: 1, c: 2 }]);
  push("array grown in two transactions", [first, Y.encodeStateAsUpdate(d, Y.encodeStateVectorFromUpdate(first))], d);
}
// The same, with values that keep no order on one side of the merge.
function grown(name, before, after) {
  const d = new Y.Doc();
  d.clientID = 1;
  const a = d.getArray("a");
  a.insert(0, before);
  const first = Y.encodeStateAsUpdate(d);
  a.insert(before.length, after);
  push(name, [first, Y.encodeStateAsUpdate(d, Y.encodeStateVectorFromUpdate(first))], d);
}
grown("array grown in two transactions: plain values, then objects", [1, 2], [{ d: 1, c: 2 }, 3]);
grown("array grown in two transactions: objects, then plain values", [{ b: 1, a: 2 }, 1], [2, 3]);

// A second client restores a format value around new content: the marker
// that restores it must write the value as the first client did.
function restore(name, op, from = 0, link = { title: "t", href: "h" }) {
  const a = new Y.Doc();
  a.clientID = 1;
  const t = a.getText("t");
  t.insert(0, "abcd");
  t.format(from, 4 - from, { link });
  const ua = Y.encodeStateAsUpdate(a);
  const b = new Y.Doc();
  b.clientID = 2;
  Y.applyUpdate(b, ua);
  op(b.getText("t"));
  push(name, [ua, Y.encodeStateAsUpdate(b, Y.encodeStateVectorFromUpdate(ua))], b);
}
restore("insert without the link inside a link", (t) => t.insert(2, "X", { link: null }));
restore("link removed from the middle of a link", (t) => t.format(1, 2, { link: null }));
// The link starts inside the range: its opening marker is deleted there
// and its value restored after the range.
restore("link removed over where a link starts", (t) => t.format(1, 2, { link: null }), 2);
// A format value that is an array, with the object inside it.
restore("array link removed from the middle of a link", (t) => t.format(1, 2, { link: null }), 0, [{ title: "t", href: "h" }]);

// The second client's objects arrive before the first client's edit they
// depend on, so they wait as pending structs.
{
  const a = new Y.Doc();
  a.clientID = 1;
  a.getArray("a").insert(0, [{ b: 1, a: 2 }]);
  const ua = Y.encodeStateAsUpdate(a);
  const b = new Y.Doc();
  b.clientID = 2;
  Y.applyUpdate(b, ua);
  b.getArray("a").insert(1, [{ d: 1, c: 2 }, { f: 1, e: 2 }]);
  const ub = Y.encodeStateAsUpdate(b, Y.encodeStateVectorFromUpdate(ua));
  push("objects waiting for their dependency", [ub, ua], b);
}

// The second client's objects wait twice, in overlapping pieces: the
// second piece is cut to the part the first does not hold.
{
  const a = new Y.Doc();
  a.clientID = 1;
  a.getArray("a").insert(0, [{ b: 1, a: 2 }]);
  const ua = Y.encodeStateAsUpdate(a);
  const b = new Y.Doc();
  b.clientID = 2;
  Y.applyUpdate(b, ua);
  const arr = b.getArray("a");
  arr.insert(1, [{ d: 1, c: 2 }, { f: 1, e: 2 }]);
  const svA = Y.encodeStateVectorFromUpdate(ua);
  const piece1 = Y.encodeStateAsUpdate(b, svA); // clocks 0-1 of client 2
  arr.insert(3, [{ h: 1, g: 2 }]); // merged into the same item
  const sv = new Map(Y.decodeStateVector(svA));
  sv.set(2, 1);
  const piece2 = Y.encodeStateAsUpdate(b, Y.encodeStateVector(new Map(sv)) ); // clocks 1-2
  push("objects waiting in overlapping pieces", [piece1, piece2, ua], b);
}

// Foreign bytes: an object yjs never writes, spliced into a yjs update in
// place of a placeholder value.
function objectBytes(entries) {
  const e = encoding.createEncoder();
  encoding.writeUint8(e, 118);
  encoding.writeVarUint(e, entries.length);
  for (const [k, v] of entries) {
    encoding.writeVarString(e, k);
    encoding.writeAny(e, v);
  }
  return encoding.toUint8Array(e);
}
function splice(update, placeholder, replacement) {
  const hex = toHex(update), p = toHex(placeholder);
  if (hex.indexOf(p) < 0 || hex.indexOf(p) !== hex.lastIndexOf(p)) throw new Error("placeholder not found once");
  return Uint8Array.from(hex.replace(p, toHex(replacement)).match(/../g).map((h) => parseInt(h, 16)));
}
function foreign(name, entries) {
  const d = new Y.Doc();
  d.clientID = 1;
  d.getMap("m").set("k", { placeholder: 12345 });
  const u = splice(Y.encodeStateAsUpdate(d), objectBytes([["placeholder", 12345]]), objectBytes(entries));
  const r = new Y.Doc();
  Y.applyUpdate(r, u);
  push(name, [u], r);
}
foreign("foreign: array-index keys after the others", [["b", 1], ["10", 2], ["a", 3], ["9", 4]]);
// The array-index key moves ahead of the object before it, whose order
// must stay with its key.
foreign("foreign: an object before an array-index key", [["b", { y: 1, x: 2 }], ["10", 2], ["a", 1]]);
foreign("foreign: a repeated key", [["a", 1], ["b", 2], ["a", { y: 1, x: 2 }]]);
// The last value's order counts, here the sorted one.
foreign("foreign: a repeated key whose last value is sorted", [["k", { b: 1, a: 2 }], ["z", 0], ["k", { a: 3, b: 4 }]]);
// More repeated keys than decoding keeps inline, each value with an order
// of its own; the last value decides, unsorted, sorted or plain.
{
  const entries = [];
  for (let i = 19; i >= 0; i--) entries.push(["k" + String(i).padStart(2, "0"), { y: i, x: i }]);
  entries.push(["k05", { q: 1, p: 2 }], ["k07", { a: 1, b: 2 }], ["k09", 0], ["zz", { n: 1, m: 2 }]);
  foreign("foreign: repeated keys past the inline ones", entries);
}
// A V1 embed whose JSON text was not written by JSON.stringify.
function foreignJSON(name, text) {
  const d = new Y.Doc();
  d.clientID = 1;
  d.getText("t").insertEmbed(0, { placeholder: 12345 });
  const enc = (s) => { const e = encoding.createEncoder(); encoding.writeVarString(e, s); return encoding.toUint8Array(e); };
  const u = splice(Y.encodeStateAsUpdate(d), enc('{"placeholder":12345}'), enc(text));
  const r = new Y.Doc();
  Y.applyUpdate(r, u);
  push(name, [u], r);
}
foreignJSON("foreign: embed JSON out of enumeration order", '{"b":1,"10":2,"a":{"y":1,"x":2}}');
foreignJSON("foreign: embed JSON with an object before an array-index key", '{"b":{"y":1,"x":2},"10":2,"a":1}');
// JSON.parse keeps a repeated key's first place and its last value, here
// an object with an order of its own.
foreignJSON("foreign: embed JSON with a repeated key", '{"a":{"y":1,"x":2},"b":0,"a":{"q":1,"p":2}}');
// White space, escaped keys and keys past ASCII.
foreignJSON("foreign: embed JSON with escaped keys and white space", ' { "b" : 1 ,\n "\\u0061" : { "y" : [ 1 , { "\\u00e9" : 1 , "d" : 2 } ] , "x" : 2 } , "\u00e9t\u00e9" : true , "a\\"q" : null } ');

writeFileSync(outPath, JSON.stringify({ generator: "gen-key-order.mjs (yjs 13.6.33)", scenarios }, null, 2) + "\n");
console.log(`wrote ${scenarios.length} scenarios to ${outPath}`);
