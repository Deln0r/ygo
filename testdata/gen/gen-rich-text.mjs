// Generates testdata/rich-text-fixtures.json - what yjs@13.6.32 does with a
// formatted text, as opposed to what bytes it writes.
//
// Every other fixture here pins the wire format. These pin behaviour: the
// same calls (insert with and without attributes, embeds, format, delete,
// applyDelta) run against a yjs Y.Text, and the resulting delta, string and
// length are recorded, along with the delta of the event each transaction
// fired. The Go test (rich_text_fixture_test.go) replays the
// calls through ygo's public API and compares the delta.
//
// The bytes are deliberately not compared. Two editors can agree on every
// visible attribute and still differ in how many format markers they wrote;
// the delta is what a user sees.
//
// These scenarios are not counted in the README's fixture totals while any of
// them is a known divergence (knownRichTextDivergences in the Go test): a
// fixture that ygo fails on purpose proves nothing yet.
//
// Run: node gen-rich-text.mjs (after npm install in this directory).

import { writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";
import * as Y from "yjs";

const here = dirname(fileURLToPath(import.meta.url));
const outPath = resolve(here, "..", "rich-text-fixtures.json");

// An op without an `attrs` key calls the yjs method without the argument,
// which is not the same call as passing an empty object: insert(i, s)
// inherits the formatting at i, insert(i, s, {}) clears it.
const ins = (index, text, attrs) => (attrs === undefined ? { op: "insert", index, text } : { op: "insert", index, text, attrs });
const embed = (index, value, attrs) => (attrs === undefined ? { op: "embed", index, embed: value } : { op: "embed", index, embed: value, attrs });
const fmt = (index, length, attrs) => ({ op: "format", index, length, attrs });
const del = (index, length) => ({ op: "delete", index, length });
const delta = (ops) => ({ op: "delta", delta: ops });

const bold = { bold: true };

const scenarios = [
  // Format
  { name: "format-over-inner-run", description: "format a range that contains a run of the same attribute",
    txns: [[ins(0, "abcdef"), fmt(2, 2, bold), fmt(0, 6, { bold: false })]] },
  { name: "format-clear-middle", description: "clear an attribute in the middle of a formatted run",
    txns: [[ins(0, "abcdef", bold), fmt(1, 3, { bold: null })]] },
  { name: "format-overlapping", description: "a second format overlaps the first and clears it",
    txns: [[ins(0, "abcdef"), fmt(0, 4, bold), fmt(2, 4, { italic: true, bold: null })]] },
  { name: "format-same-value", description: "format with the value already in effect",
    txns: [[ins(0, "abcdef", bold), fmt(1, 3, bold)]] },
  { name: "format-extends-run", description: "format from inside a run to past its end",
    txns: [[ins(0, "abcdef"), fmt(0, 3, bold), fmt(2, 3, bold)]] },
  { name: "format-meets-run", description: "format ends where a run with the same value starts",
    txns: [[ins(0, "abcdef"), fmt(3, 3, bold), fmt(0, 3, bold)]] },
  { name: "format-two-keys-mixed", description: "two attributes over text that already carries one of them",
    txns: [[ins(0, "abcdef"), fmt(1, 2, { italic: true }), fmt(0, 6, { bold: true, italic: null })]] },
  { name: "format-surrogate-pair", description: "format counts UTF-16 units across an emoji",
    txns: [[ins(0, "a😀b"), fmt(1, 2, bold)]] },
  { name: "format-next-transaction", description: "reformat in a second transaction",
    txns: [[ins(0, "abcdef"), fmt(2, 2, bold)], [fmt(0, 6, { bold: false })]] },

  // Insert
  { name: "insert-partial-attrs-inside-run", description: "insert with one attribute inside bold text",
    txns: [[ins(0, "hello", bold), ins(2, "X", { italic: true })]] },
  { name: "insert-empty-attrs-inside-run", description: "insert with an empty attribute object inside bold text",
    txns: [[ins(0, "hello", bold), ins(2, "X", {})]] },
  { name: "insert-plain-inside-run", description: "insert without attributes inside bold text",
    txns: [[ins(0, "hello", bold), ins(2, "Y")]] },
  { name: "insert-plain-at-run-end", description: "insert without attributes at the end of bold text",
    txns: [[ins(0, "hello", bold), ins(5, "!")]] },
  { name: "insert-plain-at-run-start", description: "insert without attributes at the start of bold text",
    txns: [[ins(0, "hello", bold), ins(0, "^")]] },
  { name: "insert-plain-at-run-end-next-transaction", description: "type at the end of bold text in a later transaction",
    txns: [[ins(0, "hello", bold)], [ins(5, "!")]] },

  // Embeds
  { name: "embed-inside-run", description: "insert an embed without attributes inside bold text",
    txns: [[ins(0, "hello", bold), embed(2, { image: "a" })]] },
  { name: "insert-after-embed", description: "insert text right after an embed",
    txns: [[ins(0, "ab"), embed(1, { image: "a" }), ins(2, "X")]] },
  { name: "delete-across-embed", description: "delete a range that holds an embed",
    txns: [[ins(0, "ab"), embed(1, { image: "a" }), del(1, 2)]] },
  { name: "format-across-embed", description: "format a range that holds an embed",
    txns: [[ins(0, "ab"), embed(1, { image: "a" }), fmt(0, 3, bold)]] },

  // Delete
  { name: "delete-across-boundary-then-insert", description: "delete across a format boundary, then insert at the seam",
    txns: [[ins(0, "abcdef"), fmt(2, 2, bold), del(1, 4), ins(1, "Z")]] },
  { name: "delete-whole-run", description: "delete a whole formatted run",
    txns: [[ins(0, "abcdef"), fmt(2, 2, bold), del(2, 2)]] },

  // applyDelta
  { name: "delta-embed-attrs", description: "delta inserts an embed with attributes",
    txns: [[delta([{ insert: "a" }, { insert: { image: "a.png" }, attributes: { link: "x" } }])]] },
  { name: "delta-insert-partial-attrs", description: "delta inserts with one attribute inside bold text",
    txns: [[ins(0, "hello", bold), delta([{ retain: 2 }, { insert: "Z", attributes: { italic: true } }])]] },
  { name: "delta-insert-plain-inside-run", description: "delta inserts without attributes inside bold text",
    txns: [[ins(0, "hello", bold), delta([{ retain: 2 }, { insert: "W" }])]] },
  { name: "delta-quill-document", description: "a Quill document delta",
    txns: [[delta([{ insert: "Hello" }, { insert: " world", attributes: bold }, { insert: "\n" }])]] },
  { name: "delta-retain-format-mixed", description: "retain with an attribute over mixed formatting",
    txns: [[ins(0, "abcdef"), fmt(1, 2, bold), delta([{ retain: 1 }, { retain: 4, attributes: { italic: true } }])]] },
  { name: "delta-retain-clear", description: "retain with a null attribute clears it",
    txns: [[ins(0, "abcdef", bold), delta([{ retain: 2 }, { retain: 2, attributes: { bold: null } }])]] },
  { name: "delta-delete-then-insert", description: "delete, then insert with attributes at the same cursor",
    txns: [[ins(0, "abcdef"), delta([{ retain: 1 }, { delete: 2 }, { insert: "XY", attributes: { italic: true } }])]] },
];

// yjs writes into the attribute objects it is handed: insertText sets every
// inherited attribute the caller did not name to null, on the caller's object.
// Each call therefore gets its own deep copy, and the recorded calls are taken
// before anything runs; otherwise the fixture would record a call nobody made.
const apply = (text, recorded) => {
  const op = structuredClone(recorded);
  switch (op.op) {
    case "insert":
      return "attrs" in op ? text.insert(op.index, op.text, op.attrs) : text.insert(op.index, op.text);
    case "embed":
      return "attrs" in op ? text.insertEmbed(op.index, op.embed, op.attrs) : text.insertEmbed(op.index, op.embed);
    case "format":
      return text.format(op.index, op.length, op.attrs);
    case "delete":
      return text.delete(op.index, op.length);
    case "delta":
      return text.applyDelta(op.delta);
    default:
      throw new Error(`unknown op ${op.op}`);
  }
};

const out = scenarios.map((sc) => {
  const recorded = structuredClone(sc.txns);
  const doc = new Y.Doc();
  doc.clientID = 1;
  const text = doc.getText("t");
  const events = [];
  text.observe((e) => events.push(e.delta));
  for (const txn of recorded) {
    doc.transact(() => {
      for (const op of txn) apply(text, op);
    });
  }
  return {
    name: sc.name,
    description: sc.description,
    txns: recorded,
    expected_delta: text.toDelta(),
    expected_string: text.toString(),
    expected_length: text.length,
    expected_events: events,
  };
});

for (const [i, sc] of scenarios.entries()) {
  if (JSON.stringify(sc.txns) !== JSON.stringify(out[i].txns)) {
    throw new Error(`${sc.name}: the recorded calls changed while yjs ran them`);
  }
}

const names = new Set();
for (const sc of out) {
  if (names.has(sc.name)) throw new Error(`duplicate scenario name ${sc.name}`);
  names.add(sc.name);
}

writeFileSync(
  outPath,
  JSON.stringify({ generator: "gen-rich-text.mjs (yjs@13.6.32)", scenarios: out }, null, 2) + "\n",
);
console.log(`wrote ${out.length} scenarios to ${outPath}`);
