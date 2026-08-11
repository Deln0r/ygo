// Generates testdata/relpos-fixtures.json — binary fixtures for
// RelativePosition (Y.encodeRelativePosition wire form), captured from
// JS Yjs (yjs@13.6.32).
//
// Each scenario builds a document, anchors a relative position with
// createRelativePositionFromTypeIndex, records the encoded rpos bytes
// and the resolved index, then applies edits and records the resolved
// index again. The Go test in relpos_fixture_test.go re-creates the
// rpos from the same (type, index, assoc) for byte parity, decodes the
// JS bytes, and checks resolution before and after the edits.
//
// Run: node gen-relpos.mjs (after npm install in this directory).

import { writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";
import * as Y from "yjs";

const here = dirname(fileURLToPath(import.meta.url));
const outPath = resolve(here, "..", "relpos-fixtures.json");

const toHex = (u8) =>
  Array.from(u8, (b) => b.toString(16).padStart(2, "0")).join("");

const scenarios = [];

// Resolve helper: absolute index or null.
const abs = (rpos, doc) => {
  const a = Y.createAbsolutePositionFromRelativePosition(rpos, doc);
  return a === null ? null : a.index;
};

// 1. Anchor mid-text, right associated. Insert before it moves it.
{
  const d = new Y.Doc();
  d.clientID = 21;
  const t = d.getText("t");
  t.insert(0, "hello world");
  const rpos = Y.createRelativePositionFromTypeIndex(t, 6, 0);
  const before = toHex(Y.encodeStateAsUpdate(d));
  const idx0 = abs(rpos, d);
  t.insert(0, "XYZ ");
  scenarios.push({
    description: "text mid anchor, assoc 0, insert before",
    root: "t", root_kind: "text",
    create: { path: [], index: 6, assoc: 0 },
    rpos_hex: toHex(Y.encodeRelativePosition(rpos)),
    update_before_hex: before,
    expected_index_before: idx0,
    update_after_hex: toHex(Y.encodeStateAsUpdate(d)),
    expected_index_after: abs(rpos, d),
  });
}

// 2. Left vs right association at the same index. Insert AT the anchor:
// the left-associated position stays with the left character.
for (const assoc of [0, -1]) {
  const d = new Y.Doc();
  d.clientID = 22;
  const t = d.getText("t");
  t.insert(0, "ab");
  const rpos = Y.createRelativePositionFromTypeIndex(t, 1, assoc);
  const before = toHex(Y.encodeStateAsUpdate(d));
  const idx0 = abs(rpos, d);
  t.insert(1, "X"); // "aXb"
  scenarios.push({
    description: `assoc ${assoc} at index 1 of "ab", insert at 1`,
    root: "t", root_kind: "text",
    create: { path: [], index: 1, assoc },
    rpos_hex: toHex(Y.encodeRelativePosition(rpos)),
    update_before_hex: before,
    expected_index_before: idx0,
    update_after_hex: toHex(Y.encodeStateAsUpdate(d)),
    expected_index_after: abs(rpos, d),
  });
}

// 3. End-of-type anchor on an EMPTY root text (tname wire case, tag 1).
{
  const d = new Y.Doc();
  d.clientID = 23;
  const t = d.getText("t");
  const rpos = Y.createRelativePositionFromTypeIndex(t, 0, 0);
  const before = toHex(Y.encodeStateAsUpdate(d));
  const idx0 = abs(rpos, d);
  t.insert(0, "xy");
  scenarios.push({
    description: "empty root text, end-of-type tname anchor",
    root: "t", root_kind: "text",
    create: { path: [], index: 0, assoc: 0 },
    rpos_hex: toHex(Y.encodeRelativePosition(rpos)),
    update_before_hex: before,
    expected_index_before: idx0,
    update_after_hex: toHex(Y.encodeStateAsUpdate(d)),
    expected_index_after: abs(rpos, d),
  });
}

// 4. Left-associated end-of-type: anchors on the LAST character and
// tracks the end as content grows after it.
{
  const d = new Y.Doc();
  d.clientID = 24;
  const t = d.getText("t");
  t.insert(0, "abc");
  const rpos = Y.createRelativePositionFromTypeIndex(t, 3, -1);
  const before = toHex(Y.encodeStateAsUpdate(d));
  const idx0 = abs(rpos, d);
  t.insert(3, "def"); // anchor sticks to 'c', stays at 3
  scenarios.push({
    description: "left-assoc end anchor on last char",
    root: "t", root_kind: "text",
    create: { path: [], index: 3, assoc: -1 },
    rpos_hex: toHex(Y.encodeRelativePosition(rpos)),
    update_before_hex: before,
    expected_index_before: idx0,
    update_after_hex: toHex(Y.encodeStateAsUpdate(d)),
    expected_index_after: abs(rpos, d),
  });
}

// 5. Anchor inside a NESTED text under a map key (type-ID wire case for
// the end-of-type variant, item case for the inner one).
{
  const d = new Y.Doc();
  d.clientID = 25;
  const m = d.getMap("m");
  const inner = new Y.Text();
  m.set("inner", inner);
  inner.insert(0, "ab");
  const rposInner = Y.createRelativePositionFromTypeIndex(inner, 1, 0);
  const rposEnd = Y.createRelativePositionFromTypeIndex(inner, 2, 0); // tag 2: type ID
  const before = toHex(Y.encodeStateAsUpdate(d));
  const innerIdx0 = abs(rposInner, d);
  const endIdx0 = abs(rposEnd, d);
  inner.insert(0, "Z"); // "Zab"
  const after = toHex(Y.encodeStateAsUpdate(d));
  scenarios.push({
    description: "nested text item anchor",
    root: "m", root_kind: "map",
    create: { path: ["inner"], index: 1, assoc: 0 },
    rpos_hex: toHex(Y.encodeRelativePosition(rposInner)),
    update_before_hex: before,
    expected_index_before: innerIdx0,
    update_after_hex: after,
    expected_index_after: abs(rposInner, d),
  });
  scenarios.push({
    description: "nested text end-of-type anchor (type ID)",
    root: "m", root_kind: "map",
    create: { path: ["inner"], index: 2, assoc: 0 },
    rpos_hex: toHex(Y.encodeRelativePosition(rposEnd)),
    update_before_hex: before,
    expected_index_before: endIdx0,
    update_after_hex: after,
    expected_index_after: abs(rposEnd, d),
  });
}

// 6. Anchor on a character that is then deleted: resolves to the
// deletion boundary.
{
  const d = new Y.Doc();
  d.clientID = 26;
  const t = d.getText("t");
  t.insert(0, "abcdef");
  const rpos = Y.createRelativePositionFromTypeIndex(t, 3, 0); // on 'd'
  const before = toHex(Y.encodeStateAsUpdate(d));
  const idx0 = abs(rpos, d);
  t.delete(2, 3); // remove "cde" -> "abf"
  scenarios.push({
    description: "anchor inside deleted range resolves to boundary",
    root: "t", root_kind: "text",
    create: { path: [], index: 3, assoc: 0 },
    rpos_hex: toHex(Y.encodeRelativePosition(rpos)),
    update_before_hex: before,
    expected_index_before: idx0,
    update_after_hex: toHex(Y.encodeStateAsUpdate(d)),
    expected_index_after: abs(rpos, d),
  });
}

// 7. Wide client ID (> 2^32): byte parity for the varint ID encoding.
{
  const d = new Y.Doc();
  d.clientID = 2 ** 33 + 5;
  const t = d.getText("t");
  t.insert(0, "hi");
  const rpos = Y.createRelativePositionFromTypeIndex(t, 1, 0);
  const before = toHex(Y.encodeStateAsUpdate(d));
  const idx0 = abs(rpos, d);
  t.insert(0, "!");
  scenarios.push({
    description: "wide (53-bit) client ID anchor",
    root: "t", root_kind: "text",
    create: { path: [], index: 1, assoc: 0 },
    rpos_hex: toHex(Y.encodeRelativePosition(rpos)),
    update_before_hex: before,
    expected_index_before: idx0,
    update_after_hex: toHex(Y.encodeStateAsUpdate(d)),
    expected_index_after: abs(rpos, d),
  });
}

// 8. Surrogate pair: indexes count UTF-16 code units, the emoji is 2.
// Anchor after the emoji (index 3 = 'b'), then insert before it.
{
  const d = new Y.Doc();
  d.clientID = 27;
  const t = d.getText("t");
  t.insert(0, "a\u{1F600}b"); // "a😀b": a=0, 😀=1-2, b=3
  const rpos = Y.createRelativePositionFromTypeIndex(t, 3, 0);
  const before = toHex(Y.encodeStateAsUpdate(d));
  const idx0 = abs(rpos, d);
  t.insert(0, "\u{1F680}"); // 🚀 = 2 units, shifts anchor to 5
  scenarios.push({
    description: "surrogate pair: UTF-16 unit indexing",
    root: "t", root_kind: "text",
    create: { path: [], index: 3, assoc: 0 },
    rpos_hex: toHex(Y.encodeRelativePosition(rpos)),
    update_before_hex: before,
    expected_index_before: idx0,
    update_after_hex: toHex(Y.encodeStateAsUpdate(d)),
    expected_index_after: abs(rpos, d),
  });
}

// 9. Array: anchor element 3 of a 5-element insert (one multi-element
// item), then delete a middle range crossing the anchor's left side.
{
  const d = new Y.Doc();
  d.clientID = 28;
  const a = d.getArray("a");
  a.insert(0, [10, 20, 30, 40, 50]);
  const rpos = Y.createRelativePositionFromTypeIndex(a, 3, 0); // on 40
  const before = toHex(Y.encodeStateAsUpdate(d));
  const idx0 = abs(rpos, d);
  a.delete(1, 2); // remove 20,30 -> [10,40,50], anchor moves to 1
  scenarios.push({
    description: "array multi-element item anchor, delete before",
    root: "a", root_kind: "array",
    create: { path: [], index: 3, assoc: 0 },
    rpos_hex: toHex(Y.encodeRelativePosition(rpos)),
    update_before_hex: before,
    expected_index_before: idx0,
    update_after_hex: toHex(Y.encodeStateAsUpdate(d)),
    expected_index_after: abs(rpos, d),
  });
}

const out = {
  generator: "yjs@13.6.32 (RelativePosition)",
  scenarios,
};

writeFileSync(outPath, JSON.stringify(out, null, 2) + "\n");
console.log(`wrote ${scenarios.length} relpos scenarios to ${outPath}`);
