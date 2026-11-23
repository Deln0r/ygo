// Generates testdata/difffuzz-fixtures.json: yjs's trace of every scenario
// in testdata/difffuzz/ (the differential fuzzer's corpus of shrunk
// reproducers), replayed by difffuzz-lib.mjs. The Go test
// (TestDifferential_Corpus) requires ygo's trace of each to be the same,
// without Node.
//
// Run: node gen-difffuzz.mjs (after npm install in this directory).

import { readdirSync, readFileSync, writeFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { run } from "./difffuzz-lib.mjs";

const here = dirname(fileURLToPath(import.meta.url));
const dir = resolve(here, "..", "difffuzz");
const outPath = process.env.OUT || resolve(here, "..", "difffuzz-fixtures.json");

const scenarios = readdirSync(dir)
  .filter((f) => f.endsWith(".json"))
  .sort()
  .map((f) => {
    const entry = JSON.parse(readFileSync(resolve(dir, f), "utf8"));
    return { name: f.replace(/\.json$/, ""), note: entry.note, scenario: entry.scenario, trace: run(entry.scenario) };
  });

writeFileSync(outPath, JSON.stringify({ generator: "gen-difffuzz.mjs (yjs 13.6.33)", scenarios }, null, 1) + "\n");
console.log(`wrote ${scenarios.length} scenarios to ${outPath}`);
