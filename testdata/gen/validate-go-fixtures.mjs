// validate-go-fixtures.mjs proves the reverse direction of binary
// protocol compatibility: bytes produced by Go's
// EncodeStateAsUpdate / EncodeStateAsUpdateV2 (captured into
// testdata/go-updates.json / testdata/go-update-v2-fixtures.json
// by cmd/gen-go-fixtures) are applied successfully by the JS
// Yjs runtime via Y.applyUpdate / Y.applyUpdateV2, and the
// resulting JS-side state matches what Go recorded as expected.
//
// Exit codes:
//   0  every scenario passed for both V1 and V2
//   1  one or more scenarios failed (details printed to stderr)
//   2  setup error (missing fixture file, invalid hex, etc.)
//
// Run: node validate-go-fixtures.mjs
//
// Requires: testdata/go-updates.json + go-update-v2-fixtures.json
// (run `go run ./cmd/gen-go-fixtures` from the module root first).

import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";
import * as Y from "yjs";

const here = dirname(fileURLToPath(import.meta.url));
const v1Path = resolve(here, "..", "go-updates.json");
const v2Path = resolve(here, "..", "go-update-v2-fixtures.json");

let failures = 0;
let passes = 0;

function hexToBytes(hex) {
  if (hex.length % 2 !== 0) throw new Error(`invalid hex length: ${hex.length}`);
  const out = new Uint8Array(hex.length / 2);
  for (let i = 0; i < out.length; i++) {
    out[i] = parseInt(hex.slice(i * 2, i * 2 + 2), 16);
  }
  return out;
}

// numericEqual compares values across the JS/JSON boundary. Go's
// json.Marshal of int64 emits a JSON integer; JSON.parse on the
// JS side produces a number; Y.Map.get() on the receiving side
// produces a number too. So integer/float comparisons reduce to
// `===` once both are numbers. Strings, bools, nulls match
// directly.
function deepEqual(a, b) {
  if (a === b) return true;
  if (a === null || b === null) return a === b;
  if (typeof a !== typeof b) return false;
  if (Array.isArray(a) && Array.isArray(b)) {
    if (a.length !== b.length) return false;
    for (let i = 0; i < a.length; i++) {
      if (!deepEqual(a[i], b[i])) return false;
    }
    return true;
  }
  if (typeof a === "object") {
    const ak = Object.keys(a).sort();
    const bk = Object.keys(b).sort();
    if (ak.length !== bk.length) return false;
    for (let i = 0; i < ak.length; i++) {
      if (ak[i] !== bk[i]) return false;
      if (!deepEqual(a[ak[i]], b[bk[i]])) return false;
    }
    return true;
  }
  return false;
}

function verifyMap(doc, rootName, expected) {
  const m = doc.getMap(rootName);
  const actual = {};
  for (const k of m.keys()) {
    const v = m.get(k);
    if (v === undefined) continue;
    actual[k] = v;
  }
  if (!deepEqual(actual, expected)) {
    return `map mismatch: actual=${JSON.stringify(actual)} expected=${JSON.stringify(expected)}`;
  }
  return null;
}

function verifyArray(doc, rootName, expected) {
  const a = doc.getArray(rootName);
  const actual = a.toArray();
  if (!deepEqual(actual, expected)) {
    return `array mismatch: actual=${JSON.stringify(actual)} expected=${JSON.stringify(expected)}`;
  }
  return null;
}

function verifyText(doc, rootName, expectedText, expectedLength) {
  const t = doc.getText(rootName);
  const gotText = t.toString();
  const gotLength = t.length;
  if (gotText !== expectedText) {
    return `text mismatch: actual=${JSON.stringify(gotText)} expected=${JSON.stringify(expectedText)}`;
  }
  if (gotLength !== expectedLength) {
    return `text length mismatch: actual=${gotLength} expected=${expectedLength}`;
  }
  return null;
}

function verifyXml(doc, rootName, expectedChildren) {
  const f = doc.getXmlFragment(rootName);
  const actual = [];
  for (const child of f.toArray()) {
    if (child instanceof Y.XmlElement) {
      actual.push({ kind: "element", name: child.nodeName });
    } else if (child instanceof Y.XmlText) {
      actual.push({ kind: "text", text: child.toString() });
    } else {
      actual.push({ kind: "unknown", type: child.constructor.name });
    }
  }
  const expected = expectedChildren || [];
  if (!deepEqual(actual, expected)) {
    return `xml children mismatch: actual=${JSON.stringify(actual)} expected=${JSON.stringify(expected)}`;
  }
  return null;
}

function validateFile(path, applyFn, label) {
  let raw;
  try {
    raw = readFileSync(path, "utf8");
  } catch (e) {
    if (e.code === "ENOENT") {
      console.error(`${label}: fixture ${path} missing — run \`go run ./cmd/gen-go-fixtures\` first`);
      process.exit(2);
    }
    throw e;
  }
  const ff = JSON.parse(raw);
  if (!Array.isArray(ff.scenarios) || ff.scenarios.length === 0) {
    console.error(`${label}: fixture has no scenarios`);
    process.exit(2);
  }

  for (const sc of ff.scenarios) {
    const bytes = hexToBytes(sc.update_hex);
    const doc = new Y.Doc();
    try {
      applyFn(doc, bytes);
      // A scenario may carry a second update: the case being proved is that
      // the first one holds blocks this document cannot integrate yet, and
      // that they are still there once the rest of the history shows up.
      if (sc.follow_up_hex) {
        applyFn(doc, hexToBytes(sc.follow_up_hex));
      }
    } catch (e) {
      console.error(`${label} ✘ ${sc.description}: applyUpdate threw: ${e.message}`);
      failures++;
      continue;
    }

    const rootKind = sc.root_kind || "map";
    let err;
    switch (rootKind) {
      case "map":
        err = verifyMap(doc, sc.root_name, sc.expected_map || {});
        break;
      case "array":
        err = verifyArray(doc, sc.root_name, sc.expected_array || []);
        break;
      case "text":
        err = verifyText(doc, sc.root_name, sc.expected_text || "", sc.expected_length || 0);
        break;
      case "xml":
        err = verifyXml(doc, sc.root_name, sc.expected_xml_children);
        break;
      default:
        err = `unknown root_kind: ${rootKind}`;
    }
    if (err) {
      console.error(`${label} ✘ ${sc.description}: ${err}`);
      failures++;
    } else {
      passes++;
    }
  }
}

validateFile(v1Path, (doc, bytes) => Y.applyUpdate(doc, bytes), "V1");
validateFile(v2Path, (doc, bytes) => Y.applyUpdateV2(doc, bytes), "V2");

console.log(`${passes} passed, ${failures} failed`);
if (failures > 0) process.exit(1);
