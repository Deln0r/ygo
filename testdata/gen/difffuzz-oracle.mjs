// The yjs side of the differential fuzzer (internal/difffuzz): a worker that
// reads one request per line on stdin, {"id": n, "scenario": {...}}, replays
// the scenario through yjs (difffuzz-lib.mjs) and writes one reply per line
// on stdout, {"id": n, "schema": 1, "trace": {...}} or {"id": n,
// "schema": 1, "error": "..."}.
//
// Run by the Go tests (TestDifferential_*); by hand:
//   echo '{"id":1,"scenario":...}' | node difffuzz-oracle.mjs

import { createInterface } from "node:readline";
import { SCHEMA, run } from "./difffuzz-lib.mjs";

const rl = createInterface({ input: process.stdin, crlfDelay: Infinity });
for await (const line of rl) {
  if (line.trim() === "") continue;
  let req;
  try {
    req = JSON.parse(line);
  } catch (e) {
    process.stdout.write(JSON.stringify({ id: null, schema: SCHEMA, error: `bad request: ${e.message}` }) + "\n");
    continue;
  }
  let reply;
  try {
    reply = { id: req.id, schema: SCHEMA, trace: run(req.scenario) };
  } catch (e) {
    reply = { id: req.id, schema: SCHEMA, error: String(e && e.stack) };
  }
  process.stdout.write(JSON.stringify(reply) + "\n");
}
