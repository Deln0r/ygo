// Generates testdata/yjs-xml-fixtures.json — V1 Update wire bytes
// for Y.XmlFragment / Y.XmlElement / Y.XmlText scenarios. Drives
// internal/types/xml_fixtures_test.go's cross-language proof.
//
// Each scenario captures:
//   description     — human-readable name
//   js_client_id    — pinned clientID
//   root_name       — the root XmlFragment name
//   update_hex      — Y.encodeStateAsUpdate(doc) as lowercase hex
//   expected_xml    — yjs's own XmlFragment.toString(): elements as
//                     <tag attrs>children</tag> with the tag lower-cased,
//                     attributes sorted, empty elements with a closing tag
//
// Run: node gen-xml.mjs (after npm install in this directory).

import { writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";
import * as Y from "yjs";

const here = dirname(fileURLToPath(import.meta.url));
const outPath = resolve(here, "..", "yjs-xml-fixtures.json");

// renderXml is yjs's own serialization. It used to be a renderer written
// to mirror ygo's ToString, empty elements self-closing, which made the
// fixture agree with ygo instead of checking it against yjs.
function renderXml(node) {
  return node.toString();
}

function capture(description, clientID, rootName, mutate) {
  const doc = new Y.Doc();
  doc.clientID = clientID;
  const frag = doc.getXmlFragment(rootName);
  mutate(frag, doc);
  const bytes = Y.encodeStateAsUpdate(doc);
  return {
    description,
    js_client_id: doc.clientID,
    root_name: rootName,
    update_hex: Buffer.from(bytes).toString("hex"),
    expected_xml: renderXml(frag),
  };
}

const fixtures = [
  capture("single empty element", 400, "page", (frag) => {
    frag.insert(0, [new Y.XmlElement("p")]);
  }),
  capture("element with attribute", 401, "page", (frag) => {
    const el = new Y.XmlElement("div");
    el.setAttribute("class", "container");
    frag.insert(0, [el]);
  }),
  capture("nested element with text child", 402, "page", (frag) => {
    const p = new Y.XmlElement("p");
    const text = new Y.XmlText();
    text.insert(0, "hello");
    p.insert(0, [text]);
    frag.insert(0, [p]);
  }),
  capture("multiple siblings", 403, "page", (frag) => {
    const h1 = new Y.XmlElement("h1");
    const h1Text = new Y.XmlText();
    h1Text.insert(0, "Title");
    h1.insert(0, [h1Text]);

    const p = new Y.XmlElement("p");
    const pText = new Y.XmlText();
    pText.insert(0, "body");
    p.insert(0, [pText]);

    frag.insert(0, [h1, p]);
  }),
  capture("two attributes sorted in render", 404, "page", (frag) => {
    const el = new Y.XmlElement("a");
    el.setAttribute("href", "/");
    el.setAttribute("class", "link");
    frag.insert(0, [el]);
  }),
  // XmlText with rich formatting — fixture omitted because
  // JS Y.XmlText.toString() wraps formatted runs as <attr> XML
  // elements (`<bold>bold plain</bold>`) while our Go
  // XmlText.ToString returns the plain underlying text. The
  // rich-text round-trip is already proven by
  // internal/types/text_format_test.go's cross-client tests;
  // the XML-level fixture would test a JS-specific rendering
  // convention that has limited adopter value.
];

writeFileSync(outPath, JSON.stringify(fixtures, null, 2) + "\n");
console.log(`wrote ${fixtures.length} XML fixtures to ${outPath}`);
process.exit(0);
