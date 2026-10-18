// Generates testdata/invalid-utf8-fixtures.json - wire bytes that hold a
// string with invalid UTF-8 at each place a string can sit, and the fact that
// yjs@13.6.33 / lib0@0.2.119 / y-protocols@1.0.7 reject them.
//
// lib0 reads every string with TextDecoder('utf-8', {fatal: true}), so one
// invalid byte makes the whole update (or awareness message, or relative
// position) fail to decode. JS cannot produce such bytes: its encoder turns
// lone surrogates into U+FFFD. So each case starts from bytes yjs wrote with a
// marker string, and replaces the marker's bytes with an invalid sequence of
// the same length, which keeps every length prefix valid. The unpatched bytes
// are recorded too, as accepted, so a case that fails for another reason
// cannot pass as a rejection.
//
// The Go test (invalid_utf8_fixture_test.go) expects ygo to reject every
// patched case with ErrInvalidUTF8 and to accept every original.
//
// Run: node gen-invalid-utf8.mjs (after npm install in this directory).

import { writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";
import * as Y from "yjs";
import * as awarenessProtocol from "y-protocols/awareness";

const here = dirname(fileURLToPath(import.meta.url));
const outPath = resolve(here, "..", "invalid-utf8-fixtures.json");

const MARKER = "Q~Q";
const markerBytes = Buffer.from(MARKER, "utf8");

// Invalid replacements of the marker's three bytes. Each is rejected by a
// fatal UTF-8 decoder and by Go's utf8.Valid.
const variants = {
  "lone-continuation": [0x80, 0x51, 0x51],
  "truncated-sequence": [0xe2, 0x82, 0x51],
  "encoded-surrogate": [0xed, 0xa0, 0x80],
  "overlong": [0xc0, 0xaf, 0x51],
  "invalid-lead": [0xff, 0x51, 0x51],
};

const CLIENT = 3141592653;
const newDoc = () => {
  const d = new Y.Doc();
  d.clientID = CLIENT;
  return d;
};

// Each site builds a document whose encoding holds MARKER exactly once at
// that site.
const sites = {
  "root-name": (d) => d.getText(MARKER).insert(0, "a"),
  "map-key": (d) => d.getMap("m").set(MARKER, 1),
  "text-content": (d) => d.getText("t").insert(0, MARKER),
  "format-key": (d) => d.getText("t").insert(0, "a", { [MARKER]: true }),
  "format-value": (d) => d.getText("t").insert(0, "a", { bold: MARKER }),
  "embed": (d) => d.getText("t").insertEmbed(0, { image: MARKER }),
  "any-string": (d) => d.getArray("a").insert(0, [MARKER]),
  "any-object-key": (d) => d.getArray("a").insert(0, [{ [MARKER]: 1 }]),
  "xml-element-name": (d) => d.getXmlFragment("x").insert(0, [new Y.XmlElement(MARKER.replace("~", "-"))]),
  "xml-attribute-name": (d) => {
    const el = new Y.XmlElement("p");
    d.getXmlFragment("x").insert(0, [el]);
    el.setAttribute(MARKER, "v");
  },
  "subdocument-guid": (d) => d.getMap("m").set("sub", new Y.Doc({ guid: MARKER })),
};

// Every occurrence is patched: a format key, for one, is written by the
// opening and by the closing marker.
const patch = (bytes, marker, replacement) => {
  const buf = Buffer.from(bytes);
  let at = buf.indexOf(marker);
  if (at < 0) throw new Error("marker not found");
  for (; at >= 0; at = buf.indexOf(marker, at + marker.length)) {
    Buffer.from(replacement).copy(buf, at);
  }
  return buf;
};

const rejects = (fn) => {
  try {
    fn();
    return false;
  } catch {
    return true;
  }
};

const cases = [];
const add = (name, kind, original, marker, check) => {
  if (rejects(() => check(original))) throw new Error(`${name}: yjs rejects the original bytes`);
  cases.push({ name: `${name}/original`, kind, bytes: Buffer.from(original).toString("hex"), js_rejects: false });
  for (const [variant, replacement] of Object.entries(variants)) {
    const patched = patch(original, marker, replacement);
    const rejected = rejects(() => check(patched));
    if (!rejected) throw new Error(`${name}/${variant}: yjs accepted the patched bytes`);
    cases.push({ name: `${name}/${variant}`, kind, bytes: patched.toString("hex"), js_rejects: true });
  }
};

for (const [site, build] of Object.entries(sites)) {
  const d = newDoc();
  build(d);
  const marker = site === "xml-element-name" ? Buffer.from(MARKER.replace("~", "-"), "utf8") : markerBytes;
  add(`v1/${site}`, "update-v1", Y.encodeStateAsUpdate(d), marker, (b) => Y.applyUpdate(new Y.Doc(), b));
  add(`v2/${site}`, "update-v2", Y.encodeStateAsUpdateV2(d), marker, (b) => Y.applyUpdateV2(new Y.Doc(), b));
}

// Awareness: the state travels as a JSON string.
{
  const d = newDoc();
  const aw = new awarenessProtocol.Awareness(d);
  aw.setLocalState({ user: MARKER });
  const bytes = awarenessProtocol.encodeAwarenessUpdate(aw, [d.clientID]);
  // An Awareness runs an interval timer until it is destroyed.
  add("awareness/state", "awareness", bytes, markerBytes, (b) => {
    const other = new awarenessProtocol.Awareness(newDoc());
    try {
      awarenessProtocol.applyAwarenessUpdate(other, b, "test");
    } finally {
      other.destroy();
    }
  });
  aw.destroy();
}

// Relative position into an empty root type: it carries the type's name.
{
  const d = newDoc();
  const rp = Y.createRelativePositionFromTypeIndex(d.getText(MARKER), 0);
  const bytes = Y.encodeRelativePosition(rp);
  add("relpos/type-name", "relpos", bytes, markerBytes, (b) => Y.decodeRelativePosition(b));
}

// V2 concatenates a column's strings before encoding them, so a root name
// that ends in a lone high surrogate followed by text that starts with a lone
// low surrogate travels as one valid emoji and comes back as the two halves.
// yjs accepts it; a Go string cannot hold a lone surrogate, so ygo refuses
// it (ErrSplitSurrogate) rather than change the name. V1 encodes each string
// on its own and writes the lone surrogates as U+FFFD, which both accept. A
// whole emoji in V2 is the positive control.
{
  const d = newDoc();
  d.getText("\ud83d").insert(0, "\ude00");
  const v2 = Y.encodeStateAsUpdateV2(d);
  if (rejects(() => Y.applyUpdateV2(new Y.Doc(), v2))) throw new Error("yjs rejects the split-surrogate V2 update");
  cases.push({ name: "v2/split-surrogate/root-name-and-text", kind: "update-v2", bytes: Buffer.from(v2).toString("hex"), js_rejects: false, ygo_error: "split-surrogate" });
  const v1 = Y.encodeStateAsUpdate(d);
  const back = new Y.Doc();
  Y.applyUpdate(back, v1);
  cases.push({ name: "v1/split-surrogate/root-name-and-text", kind: "update-v1", bytes: Buffer.from(v1).toString("hex"), js_rejects: false, expect_text: { root: "\ufffd", text: back.getText("\ufffd").toString() } });
  const e = newDoc();
  e.getText("t").insert(0, "a\u{1F600}b");
  cases.push({ name: "v2/whole-emoji", kind: "update-v2", bytes: Buffer.from(Y.encodeStateAsUpdateV2(e)).toString("hex"), js_rejects: false, expect_text: { root: "t", text: "a\u{1F600}b" } });
}

writeFileSync(
  outPath,
  JSON.stringify({ generator: "gen-invalid-utf8.mjs (yjs@13.6.33, lib0@0.2.119, y-protocols@1.0.7)", cases }, null, 2) + "\n",
);
console.log(`wrote ${cases.length} cases to ${outPath}`);
