// Generates testdata/yjs-update-v2-fixtures.json — V2 Update wire-byte
// fixtures captured from JS Yjs (yjs@13.6.32). Drives the V2 binary-
// protocol-compat proof in internal/encoding/v2_fixtures_test.go.
//
// Schema mirrors gen-yjs-update.mjs (V1) so the consuming Go test
// can share the fixtureFile struct (only the encoder under test
// differs).
//
// Run: node gen-yjs-update-v2.mjs (after npm install in this dir).

import { writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";
import * as Y from "yjs";

const here = dirname(fileURLToPath(import.meta.url));
const outPath = resolve(here, "..", "yjs-update-v2-fixtures.json");

// captureMap / captureArray / captureText: same shape as the V1
// generator but call Y.encodeStateAsUpdateV2 instead. Pin clientID
// for byte-stable fixtures (CI's git-diff drift check depends on it).
function captureMap(description, rootName, clientID, mutate) {
  const doc = new Y.Doc();
  doc.clientID = clientID;
  const map = doc.getMap(rootName);
  mutate(map, doc);
  const bytes = Y.encodeStateAsUpdateV2(doc);
  const expected = {};
  for (const key of map.keys()) {
    const v = map.get(key);
    if (v === undefined) continue;
    expected[key] = v;
  }
  return {
    description,
    js_client_id: doc.clientID,
    root_kind: "map",
    root_name: rootName,
    update_hex: Buffer.from(bytes).toString("hex"),
    expected_map: expected,
  };
}

function captureArray(description, rootName, clientID, mutate) {
  const doc = new Y.Doc();
  doc.clientID = clientID;
  const arr = doc.getArray(rootName);
  mutate(arr, doc);
  const bytes = Y.encodeStateAsUpdateV2(doc);
  return {
    description,
    js_client_id: doc.clientID,
    root_kind: "array",
    root_name: rootName,
    update_hex: Buffer.from(bytes).toString("hex"),
    expected_array: arr.toArray(),
  };
}

function captureText(description, rootName, clientID, mutate) {
  const doc = new Y.Doc();
  doc.clientID = clientID;
  const text = doc.getText(rootName);
  mutate(text, doc);
  const bytes = Y.encodeStateAsUpdateV2(doc);
  return {
    description,
    js_client_id: doc.clientID,
    root_kind: "text",
    root_name: rootName,
    update_hex: Buffer.from(bytes).toString("hex"),
    expected_text: text.toString(),
    expected_length: text.length,
  };
}

// Mirror the V1 scenario list. Identical clientIDs so V1 and V2
// fixtures cover the same logical surface. Extra scenarios at the
// end exercise V2's RLE compression wins (many-clients runs,
// repeated map keys, large arrays of small inserts).
const scenarios = [
  // --- Map scenarios -----------------------------------------------------
  captureMap("empty doc, no ops", "x", 100, () => {}),

  captureMap("single Map.set string", "settings", 101, (m) => {
    m.set("color", "red");
  }),

  captureMap("two Map.set on different keys", "settings", 102, (m) => {
    m.set("color", "red");
    m.set("lang", "go");
  }),

  captureMap("Map.set across multiple value types", "x", 103, (m) => {
    m.set("s", "hello");
    m.set("i", 42);
    m.set("f", 3.14);
    m.set("b_true", true);
    m.set("b_false", false);
    m.set("nullval", null);
  }),

  captureMap("Map.set + Map.set same key (LWW)", "x", 104, (m) => {
    m.set("k", "first");
    m.set("k", "second");
    m.set("k", "third");
  }),

  captureMap("Map.set + Map.delete", "x", 105, (m) => {
    m.set("a", "alpha");
    m.set("b", "beta");
    m.delete("a");
  }),

  captureMap("Map.set then delete then set again", "x", 106, (m) => {
    m.set("k", "v1");
    m.delete("k");
    m.set("k", "v2");
  }),

  captureMap("GC structs from a deleted nested type precede later content", "x", 777, (m, doc) => {
    // The reearth/ygo v1.49.1 data-loss class: with gc:true (the Y.Doc
    // default), deleting a populated nested type collapses it into GC
    // structs, and the V2 struct stream then carries those GC runs BEFORE
    // the same client's later live items. A decoder that mis-advances on a
    // GC struct silently drops everything after it. The surviving keys are
    // the assertion.
    doc.transact(() => {
      const inner = new Y.Map();
      inner.set("a", "1");
      inner.set("b", "2");
      m.set("child", inner);
      m.set("keep0", "alive0");
    });
    doc.transact(() => { m.delete("child"); });
    doc.transact(() => { m.set("keep1", "alive1"); m.set("keep2", "alive2"); });
  }),
  captureMap("Map with unicode keys and values", "x", 107, (m) => {
    m.set("ключ", "значение");
    m.set("emoji", "ok");
  }),

  // --- Array scenarios ---------------------------------------------------
  captureArray("empty Array, no ops", "x", 200, () => {}),

  captureArray("Array.push single value", "x", 201, (a) => {
    a.push(["only"]);
  }),

  captureArray("Array.push batch packed into one Item", "x", 202, (a) => {
    a.push(["a", "b", "c", "d", "e"]);
  }),

  captureArray("Array.push of various value types", "x", 203, (a) => {
    a.push(["s", 42, 3.14, true, false, null]);
  }),

  captureArray("Array.insert in middle of packed run (split)", "x", 204, (a) => {
    a.push(["a", "b", "c", "d"]);
    a.insert(2, ["X"]);
  }),

  captureArray("Array.delete single in middle", "x", 205, (a) => {
    a.push(["a", "b", "c", "d"]);
    a.delete(1, 1);
  }),

  captureArray("Array.delete range across packed run", "x", 206, (a) => {
    a.push(["a", "b", "c", "d", "e", "f"]);
    a.delete(2, 3);
  }),

  captureArray("Array sequence of separate pushes then insert", "x", 207, (a) => {
    a.push(["a"]);
    a.push(["b"]);
    a.push(["c"]);
    a.insert(2, ["X"]);
  }),

  // --- Text scenarios ----------------------------------------------------
  captureText("empty Text, no ops", "x", 300, () => {}),

  captureText("Text.insert simple ASCII", "x", 301, (t) => {
    t.insert(0, "hello");
  }),

  captureText("Text two inserts at end", "x", 302, (t) => {
    t.insert(0, "hello");
    t.insert(5, " world");
  }),

  captureText("Text insert in middle (split)", "x", 303, (t) => {
    t.insert(0, "helloworld");
    t.insert(5, " ");
  }),

  captureText("Text delete single", "x", 304, (t) => {
    t.insert(0, "hello");
    t.delete(2, 1);
  }),

  captureText("Text delete range", "x", 305, (t) => {
    t.insert(0, "hello world");
    t.delete(5, 6);
  }),

  captureText("Text Cyrillic (BMP non-ASCII)", "x", 306, (t) => {
    t.insert(0, "привет");
  }),

  captureText("Text emoji (non-BMP, surrogate pair)", "x", 307, (t) => {
    t.insert(0, "a😀b");
  }),

  captureText("Text mixed ASCII + non-BMP + insert in middle", "x", 308, (t) => {
    t.insert(0, "a😀c");
    t.insert(1, "B");
  }),

  // --- V2-flexing scenarios (highlight RLE wins) -------------------------

  // Many same-client small inserts → run encoding compresses the
  // client column massively and the leftClock column (constant +1
  // delta) compresses via IntDiffOptRle. Catches WriteKey/string
  // ordering regressions when key column gets exercised.
  captureMap("Map with many keys (RLE compression target)", "x", 400, (m) => {
    for (let i = 0; i < 20; i++) {
      m.set(`key${i}`, `val${i}`);
    }
  }),

  // Big single Array.push — ContentAny path, ContentLen column
  // gets a value of 50 for one record (vs many of 1 for individual
  // pushes — both exercised here).
  captureArray("Array large single push (ContentAny len = 50)", "x", 401, (a) => {
    const arr = [];
    for (let i = 0; i < 50; i++) arr.push(i);
    a.push(arr);
  }),

  // Long text insert — single ContentString through the string column.
  captureText("Text long single insert (string column)", "x", 402, (t) => {
    t.insert(0, "The quick brown fox jumps over the lazy dog. ".repeat(10));
  }),

  // Sequence of single-char inserts at the end — many adjacent Items
  // with constant leftClock delta = 1 → IntDiffOptRle hot path.
  captureText("Text many one-char inserts at end (constant delta)", "x", 403, (t) => {
    const s = "abcdefghijklmnopqrstuvwxyz";
    for (let i = 0; i < s.length; i++) {
      t.insert(t.length, s[i]);
    }
  }),

  // --- Edge scenarios (parity with V1 set for v1.0 fixture count) ---------
  captureMap("Map.set with empty string key", "x", 109, (m) => {
    m.set("", "empty-key-value");
    m.set("nonempty", "for contrast");
  }),

  captureArray("Array.delete entire range (empty result)", "x", 209, (a) => {
    a.push(["a", "b", "c"]);
    a.delete(0, 3);
  }),

  captureText("Text deeply nested non-BMP (combining marks + surrogates)", "x", 310, (t) => {
    t.insert(0, "é🧑‍💻");
  }),
];

const out = {
  generator: "yjs@13.6.32 (V2)",
  scenarios,
};

writeFileSync(outPath, JSON.stringify(out, null, 2) + "\n");
console.log(`wrote ${scenarios.length} V2 scenarios to ${outPath}`);
