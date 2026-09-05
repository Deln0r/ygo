// Fail if the README's fixture numbers disagree with the fixtures on disk.
//
// The counts are the project's headline claim ("the single most-important
// guarantee"), and they are exactly the kind of number that rots: adding a
// scenario is routine, updating four sentences about it is not. They had
// drifted twice - the V2 forward count was one low for months, and the reverse
// count went stale the day two scenarios were added.
//
// Exit 0 when the prose matches the JSON, 1 otherwise.
import { readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const root = resolve(here, "..", "..");

const count = (rel) => {
  const d = JSON.parse(readFileSync(resolve(root, rel), "utf8"));
  const list = Array.isArray(d) ? d : d.scenarios;
  if (!Array.isArray(list)) throw new Error(`${rel}: no scenarios array`);
  return list.length;
};

const v1Forward = count("testdata/yjs-updates.json");
const v2Forward = count("testdata/yjs-update-v2-fixtures.json");
const reverse = count("testdata/go-updates.json") + count("testdata/go-update-v2-fixtures.json");
const feature = [
  "testdata/yjs-xml-fixtures.json",
  "testdata/awareness-fixtures.json",
  "testdata/sync-fixtures.json",
  "testdata/undo-fixtures.json",
  "testdata/snapshot-fixtures.json",
  "testdata/subdoc-fixtures.json",
  "testdata/wire-edge-fixtures.json",
  "testdata/nested-gc-fixtures.json",
  "testdata/relpos-fixtures.json",
].reduce((n, f) => n + count(f), 0);
const total = v1Forward + v2Forward + reverse + feature;

const readme = readFileSync(resolve(root, "README.md"), "utf8");
const claims = [
  { what: "grand total", n: total, re: /(\d+) cross-language fixture scenarios/g },
  { what: "V1 forward", n: v1Forward, re: /\*\*(\d+) V1 forward fixtures\*\*/g },
  { what: "V2 forward", n: v2Forward, re: /\*\*(\d+) V2 forward fixtures\*\*/g },
  { what: "reverse", n: reverse, re: /\*\*(\d+) reverse fixtures\*\*/g },
  { what: "feature", n: feature, re: /\*\*(\d+) feature fixtures\*\*/g },
  { what: "status-table V1", n: v1Forward, re: /proven by (\d+) V1 \+ \d+ V2 fixture scenarios/g },
  { what: "status-table V2", n: v2Forward, re: /proven by \d+ V1 \+ (\d+) V2 fixture scenarios/g },
  { what: "status-table reverse", n: reverse, re: /proven by (\d+) reverse fixtures/g },
];

let bad = 0;
for (const c of claims) {
  const found = [...readme.matchAll(c.re)].map((m) => Number(m[1]));
  if (found.length === 0) {
    console.error(`✘ ${c.what}: the README no longer states this count (pattern ${c.re}); update this check or restore the sentence`);
    bad++;
    continue;
  }
  const wrong = found.filter((n) => n !== c.n);
  if (wrong.length) {
    console.error(`✘ ${c.what}: README says ${wrong.join(", ")}, fixtures on disk say ${c.n}`);
    bad++;
  }
}
if (bad) {
  console.error(`\n${bad} fixture-count claim(s) in README.md disagree with testdata/.`);
  process.exit(1);
}
console.log(`fixture counts agree: ${v1Forward} V1 + ${v2Forward} V2 forward, ${reverse} reverse, ${feature} feature = ${total}`);
