// Generates testdata/lib0.json — encoding fixtures for ygo's internal/lib0 package.
//
// Each case captures: input value, kind discriminator, and the exact bytes the
// JS lib0 reference emits. Go tests assert byte equality on encode and value
// equality on decode.
//
// Run: `node gen-lib0.mjs` from this directory after `npm install`.

import { writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";
import * as encoding from "lib0/encoding";

const here = dirname(fileURLToPath(import.meta.url));
const outPath = resolve(here, "..", "lib0.json");

const cases = [];

function emit(kind, name, valueField, value, encode) {
  const enc = encoding.createEncoder();
  encode(enc);
  const bytes = encoding.toUint8Array(enc);
  cases.push({
    kind,
    name,
    [valueField]: value,
    bytesHex: Buffer.from(bytes).toString("hex"),
  });
}

// varuint
const varuintValues = [
  ["zero", 0n],
  ["one", 1n],
  ["max1byte", 127n],
  ["min2byte", 128n],
  ["byte255", 255n],
  ["max2byte", 16383n],
  ["min3byte", 16384n],
  ["maxUint32", 4294967295n],
  ["maxUint53", (1n << 53n) - 1n],
];
for (const [name, v] of varuintValues) {
  emit("varuint", name, "valueU64", Number(v), (enc) =>
    encoding.writeVarUint(enc, Number(v))
  );
}

// varint
const varintValues = [
  ["zero", 0],
  ["one", 1],
  ["minusOne", -1],
  ["pos63", 63],
  ["neg63", -63],
  ["pos64", 64],
  ["neg64", -64],
  ["pos8191", 8191],
  ["neg8191", -8191],
  ["pos8192", 8192],
  ["neg8192", -8192],
  ["maxInt32", 2147483647],
  ["minInt32", -2147483648],
];
for (const [name, v] of varintValues) {
  emit("varint", name, "valueI64", v, (enc) => encoding.writeVarInt(enc, v));
}

// varstring
const varstringValues = [
  ["empty", ""],
  ["a", "a"],
  ["hello", "hello"],
  ["unicode", "привет"],
  ["emoji", "👋🌍"],
];
for (const [name, v] of varstringValues) {
  emit("varstring", name, "valueStr", v, (enc) =>
    encoding.writeVarString(enc, v)
  );
}

// varbuffer (varuint8array)
const varbufferValues = [
  ["empty", new Uint8Array([])],
  ["singleByte", new Uint8Array([0x01])],
  ["small", new Uint8Array([0x00, 0x01, 0x02, 0x03, 0xff])],
  ["kilobyte", new Uint8Array(1024).fill(0xab)],
];
for (const [name, v] of varbufferValues) {
  emit("varbuffer", name, "valueBufHex", Buffer.from(v).toString("hex"), (enc) =>
    encoding.writeVarUint8Array(enc, v)
  );
}

// float32
const float32Values = [
  ["zero", 0],
  ["one", 1],
  ["minusOne", -1],
  ["pi", Math.fround(Math.PI)],
];
for (const [name, v] of float32Values) {
  emit("float32", name, "valueF32", String(v), (enc) =>
    encoding.writeFloat32(enc, v)
  );
}

// float64
const float64Values = [
  ["zero", 0],
  ["one", 1],
  ["minusOne", -1],
  ["pi", Math.PI],
  ["maxValue", Number.MAX_VALUE],
];
for (const [name, v] of float64Values) {
  emit("float64", name, "valueF64", String(v), (enc) =>
    encoding.writeFloat64(enc, v)
  );
}

const out = {
  generator: "lib0@0.2.118",
  cases,
};

writeFileSync(outPath, JSON.stringify(out, null, 2) + "\n");
console.log(`wrote ${cases.length} cases to ${outPath}`);
