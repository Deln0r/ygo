// Generates testdata/v2-dense-fixtures.json: V2 states that hold more
// structs than bytes, as yjs 13.6.33 writes them. V2 run-length encodes the
// info, client, clock, length and type-ref columns, so structs without
// bytes of their own (GC, deleted content, empty nested types) cost almost
// nothing: a uniform run of them is ~40 bytes whatever its length. The Go
// test (v2_dense_fixture_test.go) applies each with ApplyUpdateV2 and
// requires yjs's bytes back, and requires a state past ygo's struct budget
// to be refused with ErrUpdateTooDense.
//
// Run: node gen-v2-dense.mjs (after npm install in this directory).

import { writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";
import * as Y from "yjs";

const here = dirname(fileURLToPath(import.meta.url));
const outPath = process.env.OUT || resolve(here, "..", "v2-dense-fixtures.json");
const toHex = (u8) => Array.from(u8, (b) => b.toString(16).padStart(2, "0")).join("");

const scenarios = [];
// refused: ygo refuses the state (ErrUpdateTooDense); early: before
// decoding any struct (the first client's count alone is past the budget;
// a later client's count is read only after the clients before it).
function scenario(name, build, refused = false, early = false, encoder = null) {
  const d = new Y.Doc();
  d.clientID = 1;
  build(d);
  const state = encoder ? Y.encodeStateAsUpdateV2(d, undefined, encoder) : Y.encodeStateAsUpdateV2(d);
  const structs = Y.decodeUpdateV2(state).structs.length;
  if (!encoder && structs <= state.length) throw new Error(`${name}: ${structs} structs in ${state.length} bytes is not dense`);
  const r = new Y.Doc();
  r.clientID = 99;
  Y.applyUpdateV2(r, state);
  scenarios.push({
    name,
    structs,
    refused,
    early,
    state: toHex(state),
    // What a receiver writes back.
    roundTrip: toHex(Y.encodeStateAsUpdateV2(r)),
  });
}

const prepends = (n) => (d) => {
  const t = d.getText("t");
  for (let i = 0; i < n; i++) t.insert(0, "x");
  t.delete(0, t.length);
};

scenario("10k empty nested arrays", (d) => d.transact(() => { const a = d.getArray("a"); for (let i = 0; i < 10000; i++) a.push([new Y.Array()]); }));
scenario("10k empty nested maps prepended", (d) => { const a = d.getArray("a"); for (let i = 0; i < 10000; i++) a.insert(0, [new Y.Map()]); });
scenario("10k nested maps prepended, all deleted", (d) => { const a = d.getArray("a"); for (let i = 0; i < 10000; i++) a.insert(0, [new Y.Map()]); a.delete(0, a.length); });
scenario("100k characters prepended, all deleted", prepends(100000));
scenario("1M characters prepended, all deleted", prepends(1000000));
scenario("two clients typing in turns, all deleted", (d) => {
  const b = new Y.Doc();
  b.clientID = 2;
  for (let i = 0; i < 10000; i++) {
    const [w, r] = i % 2 ? [b, d] : [d, b];
    w.getText("t").insert(w.getText("t").length, "x");
    Y.applyUpdateV2(r, Y.encodeStateAsUpdateV2(w, Y.encodeStateVector(r)));
  }
  d.getText("t").delete(0, d.getText("t").length);
});
// Past ygo's budget of max(bytes, 1<<20) structs per update: a valid yjs
// state ygo refuses on purpose.
scenario("1.1M characters prepended, all deleted (past the budget)", prepends(1100000), true, true);
// Two clients each under the budget, together past it: the budget is per
// update, not per client.
scenario("two clients with 600k each, past the budget together", (d) => {
  prepends(600000)(d);
  d.clientID = 2;
  const u = d.getText("u");
  for (let i = 0; i < 600000; i++) u.insert(0, "y");
  u.delete(0, u.length);
}, true);

// Cached keys: yjs's V2 decoder reads a key once and then by its clock
// (readKey), though its encoder leaves the cache off (writeKey, "uncomment
// to introduce this feature finally"). An encoder with the cache on writes
// a format key once for 50 markers; a reader writing the document again
// writes it each time, so ygo counts the bytes a cached key returns
// against the same budget as structs. This one is within it.
class CachingEncoderV2 extends Y.UpdateEncoderV2 {
  writeKey(key) {
    const clock = this.keyMap.get(key);
    if (clock === undefined) {
      this.keyMap.set(key, this.keyClock);
      this.keyClockEncoder.write(this.keyClock++);
      this.stringEncoder.write(key);
    } else {
      this.keyClockEncoder.write(clock);
    }
  }
}
scenario("one cached format key for 50 markers", (d) => {
  const t = d.getText("t");
  t.insert(0, "x".repeat(100));
  for (let i = 0; i < 100; i += 2) t.format(i, 1, { ["k".repeat(1000)]: true });
}, false, false, new CachingEncoderV2());

writeFileSync(outPath, JSON.stringify({ generator: "gen-v2-dense.mjs (yjs 13.6.33)", scenarios }, null, 2) + "\n");
console.log(`wrote ${scenarios.length} scenarios to ${outPath}`);
