// Generates testdata/v8-sort-fixtures.json: the order Node's V8 gives an
// array sorted with a comparator shaped like yjs mergeUpdatesV2's: keys
// ascending, 0 at an equal key for the same kind, and -1 for different kinds
// whichever is first, which is not consistent, so the order depends on
// V8's algorithm (binary insertion below 8 elements, TimSort with galloping
// above). The Go test (internal/encoding v8sort_fixture_test.go) sorts the
// same arrays with its port and requires the same order.
//
// Run: node gen-v8-sort.mjs (after npm install in this directory).
import { writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";

const here = dirname(fileURLToPath(import.meta.url));
const outPath = process.env.OUT || resolve(here, "..", "v8-sort-fixtures.json");

function rng(seed) {
  let s = seed >>> 0;
  return () => {
    s = (s + 0x6d2b79f5) >>> 0;
    let t = s;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}
const cmp = (a, b) => (a.k === b.k ? (a.t === b.t ? 0 : -1) : a.k - b.k);
const cases = [];
const sizes = [1, 2, 3, 5, 7, 8, 9, 31, 32, 33, 63, 64, 65, 66, 100, 127, 128, 129, 200, 257, 300];
let seed = 1;
for (const n of sizes) {
  for (const keys of [2, 4, 16, 1000]) {
    for (const shape of ["random", "ascending", "descending", "runs"]) {
      const r = rng(seed++);
      const arr = Array.from({ length: n }, (_, i) => {
        let k;
        if (shape === "random") k = Math.floor(r() * keys);
        else if (shape === "ascending") k = Math.floor((i * keys) / n);
        else if (shape === "descending") k = keys - 1 - Math.floor((i * keys) / n);
        else k = (i % 23) < 12 ? Math.floor((i * keys) / n) : Math.floor(r() * keys);
        return { k, t: r() < 0.5 ? 0 : 1, i };
      });
      const input = arr.map(({ k, t }) => [k, t]);
      const sorted = [...arr].sort(cmp);
      cases.push({ n, keys, shape, input, order: sorted.map((e) => e.i) });
    }
  }
}
writeFileSync(outPath, JSON.stringify({ node: process.versions.node, v8: process.versions.v8, cases }) + "\n");
console.log(`wrote ${cases.length} cases (V8 ${process.versions.v8}) to ${outPath}`);
