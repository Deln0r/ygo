// Generates testdata/yjs-updates.json — V1 Update wire-byte fixtures
// captured from JS Yjs (yjs@13.6.32). Drives the binary-protocol-compat
// proof in internal/encoding/fixture_test.go.
//
// For each scenario we capture:
//   description    — human-readable name
//   js_client_id   — the clientID Y.Doc chose (debug info; not used by tests)
//   root_name      — the root Y.Map name we set
//   update_hex     — Y.encodeStateAsUpdate(doc) as lowercase hex
//   expected_map   — final live map state as a JSON-serializable object
//
// Run: node gen-yjs-update.mjs (after npm install in this directory).

import { writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";
import * as Y from "yjs";

const here = dirname(fileURLToPath(import.meta.url));
const outPath = resolve(here, "..", "yjs-updates.json");

// captureMap builds a Y.Doc with a deterministic clientID and
// captures Y.Map operations. Y.Doc has no constructor option for
// clientID, so we override the field immediately after construction
// before any mutation that would mint an ID. Pinning clientID makes
// the wire bytes reproducible so CI's `git diff --exit-code
// testdata/` catches real format drift rather than JS Yjs's random
// clientID rolls.
function captureMap(description, rootName, clientID, mutate) {
  const doc = new Y.Doc();
  doc.clientID = clientID;
  const map = doc.getMap(rootName);
  mutate(map, doc);
  const bytes = Y.encodeStateAsUpdate(doc);
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

// captureArray is the array counterpart of captureMap. Y.Array's
// JSON snapshot via toArray() yields a plain JS array of values
// already in document order; we serialize that as expected_array.
function captureArray(description, rootName, clientID, mutate) {
  const doc = new Y.Doc();
  doc.clientID = clientID;
  const arr = doc.getArray(rootName);
  mutate(arr, doc);
  const bytes = Y.encodeStateAsUpdate(doc);
  return {
    description,
    js_client_id: doc.clientID,
    root_kind: "array",
    root_name: rootName,
    update_hex: Buffer.from(bytes).toString("hex"),
    expected_array: arr.toArray(),
  };
}

// captureText is the text counterpart of captureMap / captureArray.
// Y.Text's snapshot via toString() yields the live concatenated
// plain text; we serialize that as expected_text. UTF-16 length
// (matching JS string.length) is captured as expected_length so the
// Go test verifies our utf16.Length helper agrees with JS.
function captureText(description, rootName, clientID, mutate) {
  const doc = new Y.Doc();
  doc.clientID = clientID;
  const text = doc.getText(rootName);
  mutate(text, doc);
  const bytes = Y.encodeStateAsUpdate(doc);
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

// Scenario clientIDs are arbitrary but stable. Picking distinct
// values per scenario avoids any cross-scenario state leakage in
// case the generator ever shares a doc.
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
    a.delete(1, 1); // remove "b"
  }),

  captureArray("Array.delete range across packed run", "x", 206, (a) => {
    a.push(["a", "b", "c", "d", "e", "f"]);
    a.delete(2, 3); // remove "c", "d", "e"
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
    t.delete(2, 1); // remove "l"
  }),

  captureText("Text delete range", "x", 305, (t) => {
    t.insert(0, "hello world");
    t.delete(5, 6); // remove " world"
  }),

  captureText("Text Cyrillic (BMP non-ASCII)", "x", 306, (t) => {
    t.insert(0, "привет");
  }),

  captureText("Text emoji (non-BMP, surrogate pair)", "x", 307, (t) => {
    t.insert(0, "a😀b");
  }),

  captureText("Text mixed ASCII + non-BMP + insert in middle", "x", 308, (t) => {
    t.insert(0, "a😀c");
    t.insert(1, "B"); // between a and 😀; idx in UTF-16 units
  }),

  // --- Edge / stress scenarios (push count over 100 for v1.0 bar) ----------
  captureMap("Map.set with empty string key", "x", 109, (m) => {
    m.set("", "empty-key-value");
    m.set("nonempty", "for contrast");
  }),

  captureArray("Array.delete entire range (empty result)", "x", 209, (a) => {
    a.push(["a", "b", "c"]);
    a.delete(0, 3);
  }),

  captureText("Text empty insert is no-op", "x", 309, (t) => {
    t.insert(0, "before");
    t.insert(6, "");
    t.insert(6, "-after");
  }),

  captureText("Text deeply nested non-BMP (combining marks + surrogates)", "x", 310, (t) => {
    // e + combining acute + emoji + zero-width-joiner + family modifier
    t.insert(0, "é🧑‍💻"); // 7 UTF-16 units total
  }),
];

const out = {
  generator: "yjs@13.6.32",
  scenarios,
};

writeFileSync(outPath, JSON.stringify(out, null, 2) + "\n");
console.log(`wrote ${scenarios.length} scenarios to ${outPath}`);
