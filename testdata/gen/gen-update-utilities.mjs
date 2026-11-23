// Generates testdata/update-utility-fixtures.json: what yjs 13.6.33's
// convertUpdateFormatV1ToV2 / V2ToV1, encodeStateVectorFromUpdate(V2) and
// parseUpdateMeta(V2) give
//   - "derived": for every input and merge of testdata/merge-update-fixtures.json
//     (referenced by scenario index and slot, not copied), and
//   - "scenarios": for updates written by hand in V1 and in V2 (V2 through
//     yjs's UpdateEncoderV2, not a conversion), covering what yjs's update
//     writers never produce: clients ascending, a client in two separated
//     sections, adjacent sections with a gap or going backwards, runs not
//     starting at 0, Skips first and later, zero-length structs, delete-only
//     updates, updates without a delete set, trailing bytes, repeated,
//     overlapping and zero-length delete ranges, items with origins,
//     unknown shared type refs, nonzero and overlong V2 feature flags, a
//     negative V2 origin clock, empty structs at clock 2^53.
// A yjs error is recorded as {"err": message}. The Go test
// (update_utility_fixture_test.go) requires the same bytes from ygo.
//
// Run after gen-merge-updates.mjs: node gen-update-utilities.mjs

import { readFileSync, writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";
import * as Y from "yjs";
import * as encoding from "lib0/encoding";

const here = dirname(fileURLToPath(import.meta.url));
const outPath = process.env.OUT || resolve(here, "..", "update-utility-fixtures.json");
const toHex = (u8) => Buffer.from(u8).toString("hex");
const fromHex = (h) => new Uint8Array(Buffer.from(h, "hex"));

const attempt = (f) => {
  try {
    return { out: toHex(f()) };
  } catch (e) {
    return { err: String(e && e.message) };
  }
};
const metaOf = (f) => {
  try {
    const m = f();
    const sorted = (map) => Array.from(map.entries()).sort((a, b) => a[0] - b[0]);
    return { from: sorted(m.from), to: sorted(m.to) };
  } catch (e) {
    return { err: String(e && e.message) };
  }
};
const v1Results = (u) => ({
  convert: attempt(() => Y.convertUpdateFormatV1ToV2(u)),
  sv: attempt(() => Y.encodeStateVectorFromUpdate(u)),
  meta: metaOf(() => Y.parseUpdateMeta(u)),
});
const v2Results = (u) => ({
  convert: attempt(() => Y.convertUpdateFormatV2ToV1(u)),
  sv: attempt(() => Y.encodeStateVectorFromUpdateV2(u)),
  meta: metaOf(() => Y.parseUpdateMetaV2(u)),
});

// Every input and merge of the merge fixtures.
const merges = JSON.parse(readFileSync(resolve(here, "..", "merge-update-fixtures.json"), "utf8")).scenarios;
const derived = [];
merges.forEach((sc, i) => {
  // The first 16 inputs of a scenario (some hold 300 one-struct inputs).
  sc.inputs_v1.slice(0, 16).forEach((h, k) => derived.push({ scenario: i, slot: `input_v1/${k}`, v2: false, ...v1Results(fromHex(h)) }));
  if (sc.merge_v1) derived.push({ scenario: i, slot: "merge_v1", v2: false, ...v1Results(fromHex(sc.merge_v1)) });
  sc.inputs_v2.slice(0, 16).forEach((h, k) => derived.push({ scenario: i, slot: `input_v2/${k}`, v2: true, ...v2Results(fromHex(h)) }));
  if (sc.merge_v2) derived.push({ scenario: i, slot: "merge_v2", v2: true, ...v2Results(fromHex(sc.merge_v2)) });
});

// Updates written by hand. A section is {client, clock, structs}; a struct
// is gc(len), skip(len), str(text) under root "t", strAfter(text, client,
// clock) (an origin), anyKey(key, value) under root "m". ds is a list of
// {client, ranges: [[clock, len], ...]} written as given, or null for no
// delete set at all; tail is bytes appended.
const gc = (len) => ({ kind: "gc", len });
const skip = (len) => ({ kind: "skip", len });
const str = (text) => ({ kind: "str", text });
const strAfter = (text, client, clock) => ({ kind: "str", text, origin: [client, clock] });
const anyKey = (key, value) => ({ kind: "any", key, value });
const type = (ref) => ({ kind: "type", ref });

function writeV1(sections, ds, tail = []) {
  const e = encoding.createEncoder();
  encoding.writeVarUint(e, sections.length);
  for (const sec of sections) {
    encoding.writeVarUint(e, sec.structs.length);
    encoding.writeVarUint(e, sec.client);
    encoding.writeVarUint(e, sec.clock);
    for (const st of sec.structs) {
      switch (st.kind) {
        case "gc": encoding.writeUint8(e, 0); encoding.writeVarUint(e, st.len); break;
        case "skip": encoding.writeUint8(e, 10); encoding.writeVarUint(e, st.len); break;
        case "str":
          if (st.origin) {
            encoding.writeUint8(e, 0x84);
            encoding.writeVarUint(e, st.origin[0]);
            encoding.writeVarUint(e, st.origin[1]);
          } else {
            encoding.writeUint8(e, 4);
            encoding.writeVarUint(e, 1);
            encoding.writeVarString(e, "t");
          }
          encoding.writeVarString(e, st.text);
          break;
        case "any":
          encoding.writeUint8(e, 0x28);
          encoding.writeVarUint(e, 1);
          encoding.writeVarString(e, "m");
          encoding.writeVarString(e, st.key);
          encoding.writeVarUint(e, 1);
          encoding.writeAny(e, st.value);
          break;
        case "type":
          encoding.writeUint8(e, 7);
          encoding.writeVarUint(e, 1);
          encoding.writeVarString(e, "t");
          encoding.writeVarUint(e, st.ref);
          break;
      }
    }
  }
  if (ds !== null) {
    encoding.writeVarUint(e, ds.length);
    for (const { client, ranges } of ds) {
      encoding.writeVarUint(e, client);
      encoding.writeVarUint(e, ranges.length);
      for (const [clock, len] of ranges) {
        encoding.writeVarUint(e, clock);
        encoding.writeVarUint(e, len);
      }
    }
  }
  for (const b of tail) encoding.writeUint8(e, b);
  return encoding.toUint8Array(e);
}

function writeV2(sections, ds, tail = []) {
  const enc = new Y.UpdateEncoderV2();
  encoding.writeVarUint(enc.restEncoder, sections.length);
  for (const sec of sections) {
    encoding.writeVarUint(enc.restEncoder, sec.structs.length);
    enc.writeClient(sec.client);
    encoding.writeVarUint(enc.restEncoder, sec.clock);
    for (const st of sec.structs) {
      switch (st.kind) {
        case "gc": enc.writeInfo(0); enc.writeLen(st.len); break;
        case "skip": enc.writeInfo(10); encoding.writeVarUint(enc.restEncoder, st.len); break;
        case "str":
          if (st.origin) {
            enc.writeInfo(0x84);
            enc.writeLeftID(Y.createID(st.origin[0], st.origin[1]));
          } else {
            enc.writeInfo(4);
            enc.writeParentInfo(true);
            enc.writeString("t");
          }
          enc.writeString(st.text);
          break;
        case "any":
          enc.writeInfo(0x28);
          enc.writeParentInfo(true);
          enc.writeString("m");
          enc.writeString(st.key);
          enc.writeLen(1);
          enc.writeAny(st.value);
          break;
        case "type":
          enc.writeInfo(7);
          enc.writeParentInfo(true);
          enc.writeString("t");
          enc.writeTypeRef(st.ref);
          break;
      }
    }
  }
  if (ds !== null) {
    encoding.writeVarUint(enc.restEncoder, ds.length);
    for (const { client, ranges } of ds) {
      enc.resetDsCurVal();
      encoding.writeVarUint(enc.restEncoder, client);
      encoding.writeVarUint(enc.restEncoder, ranges.length);
      for (const [clock, len] of ranges) {
        enc.writeDsClock(clock);
        enc.writeDsLen(len);
      }
    }
  }
  const out = enc.toUint8Array();
  return tail.length ? new Uint8Array([...out, ...tail]) : out;
}

const sec = (client, clock, ...structs) => ({ client, clock, structs });
const cases = [
  ["empty", [], []],
  ["no delete set", [sec(1, 0, gc(1))], null],
  ["a Skip first", [sec(1, 0, skip(5))], []],
  ["a Skip opening a later client's run", [sec(5, 0, gc(2)), sec(3, 0, skip(4), gc(1))], []],
  ["a Skip later in a run", [sec(1, 0, gc(2), skip(3), gc(1))], []],
  ["clients ascending", [sec(1, 0, gc(2)), sec(5, 0, gc(3))], []],
  ["a client in two separated sections", [sec(5, 0, gc(2)), sec(3, 0, gc(1)), sec(5, 2, gc(2))], []],
  ["a client in two runs both from 0", [sec(5, 0, gc(2)), sec(3, 0, gc(1)), sec(5, 0, gc(3))], []],
  ["adjacent sections with a gap", [sec(1, 0, gc(2)), sec(1, 5, gc(1))], []],
  ["adjacent sections going backwards", [sec(1, 5, gc(1)), sec(1, 0, gc(2))], []],
  ["a run not starting at 0", [sec(1, 3, gc(2)), sec(2, 0, gc(1))], []],
  ["zero-length structs", [sec(1, 0, gc(0), gc(2), skip(0))], []],
  ["delete only", [], [{ client: 7, ranges: [[3, 1]] }, { client: 1, ranges: [[0, 2]] }]],
  ["trailing bytes", [sec(1, 0, gc(2))], [], [7, 7]],
  ["delete ranges repeated and overlapping", [sec(1, 0, gc(9))], [{ client: 1, ranges: [[5, 2], [1, 3]] }, { client: 1, ranges: [[2, 2]] }]],
  ["items with and without origins", [sec(4, 0, str("ab"), strAfter("c", 4, 1), anyKey("k", 1), anyKey("k2", "v"))], []],
  ["two clients with items", [sec(9, 0, str("x"), gc(1)), sec(4, 0, strAfter("y", 9, 0))], [{ client: 9, ranges: [[1, 1]] }]],
  ["shared type refs 0 and 6", [sec(1, 0, type(0), type(6))], []],
  ["shared type ref 7", [sec(1, 0, type(7))], []],
  ["shared type ref 256", [sec(1, 0, type(256))], []],
  ["an empty struct at clock 2^53", [sec(1, 0, gc(2 ** 53 - 1), gc(1), gc(0))], []],
  ["a section of an empty struct starting at 2^53", [sec(1, 0, gc(2 ** 53)), sec(1, 2 ** 53, gc(0))], []],
];
const scenarios = [];
for (const [name, sections, ds, tail] of cases) {
  const v1 = writeV1(sections, ds, tail || []);
  scenarios.push({ name, v2: false, input: toHex(v1), ...v1Results(v1) });
  const v2 = writeV2(sections, ds, tail || []);
  scenarios.push({ name, v2: true, input: toHex(v2), ...v2Results(v2) });
}
// V1 only: a zero-length delete range (V2 cannot write one).
{
  const v1 = writeV1([sec(1, 0, gc(4))], [{ client: 1, ranges: [[3, 0], [0, 1]] }]);
  scenarios.push({ name: "a zero-length delete range", v2: false, input: toHex(v1), ...v1Results(v1) });
}
// Nothing but a 0 byte: no structs and no delete set.
scenarios.push({ name: "a single 0 byte", v2: false, input: "00", ...v1Results(new Uint8Array([0])) });
// V2 only: the feature flag, which yjs reads as a varuint and ignores, as 1
// and as an overlong 0; an origin clock that the signed clock column makes
// negative (V1 cannot write one).
{
  const base = writeV2([sec(1, 0, gc(1))], []);
  const flag1 = new Uint8Array([1, ...base.slice(1)]);
  scenarios.push({ name: "a V2 feature flag of 1", v2: true, input: toHex(flag1), ...v2Results(flag1) });
  const overlong = new Uint8Array([0x80, 0x00, ...base.slice(1)]);
  scenarios.push({ name: "an overlong V2 feature flag", v2: true, input: toHex(overlong), ...v2Results(overlong) });
  const neg = writeV2([sec(1, 0, strAfter("a", 1, -1)), sec(2, 0, strAfter("b", 1, -5)), sec(3, 0, strAfter("c", 3, -(2 ** 40)))], []);
  scenarios.push({ name: "negative V2 origin clocks", v2: true, input: toHex(neg), ...v2Results(neg) });
}
// What a document holds after applying each of these, as yjs applies them.
for (const sc of scenarios) {
  if (!sc.name.includes("2^53")) continue;
  const doc = new Y.Doc();
  (sc.v2 ? Y.applyUpdateV2 : Y.applyUpdate)(doc, fromHex(sc.input));
  sc.applied_sv = toHex(Y.encodeStateVector(doc));
}

writeFileSync(outPath, JSON.stringify({ generator: "gen-update-utilities.mjs (yjs 13.6.33)", derived, scenarios }, null, 1) + "\n");
console.log(`wrote ${scenarios.length} scenarios and ${derived.length} derived cases to ${outPath}`);
