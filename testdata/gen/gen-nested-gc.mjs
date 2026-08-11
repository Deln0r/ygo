// Generates cross-language fixtures for garbage collection of deleted
// nested shared types. Each scenario builds a document containing a
// populated nested type (Map / Array, possibly multiple levels), deletes
// the reference to it, and captures the V1 encodeStateAsUpdate bytes.
//
// When yjs deletes a populated nested type it (a) turns the reference
// item into a ContentDeleted marker and (b) collapses every child into a
// garbage-collected (ref 0) struct run. The Go port must reproduce both
// to stay byte-compatible. The matching op sequences live in
// nested_gc_test.go, keyed by scenario name.
//
// Run: node testdata/gen/gen-nested-gc.mjs
import * as Y from "yjs";
import { writeFileSync } from "node:fs";

const toHex = (u8) => Buffer.from(u8).toString("hex");

const scenarios = [
  {
    name: "map_in_map",
    clientID: 21,
    build: (d) => {
      const root = d.getMap("root");
      const child = new Y.Map();
      root.set("child", child);
      child.set("a", "x");
      child.set("b", "y");
      child.set("c", "z");
      root.delete("child");
    },
  },
  {
    name: "array_in_map",
    clientID: 22,
    build: (d) => {
      const root = d.getMap("root");
      const child = new Y.Array();
      root.set("list", child);
      child.insert(0, ["p"]);
      child.insert(1, ["q"]);
      child.insert(2, ["r"]);
      root.delete("list");
    },
  },
  {
    name: "map_in_map_2level",
    clientID: 23,
    build: (d) => {
      const root = d.getMap("root");
      const outer = new Y.Map();
      root.set("outer", outer);
      const inner = new Y.Map();
      outer.set("inner", inner);
      inner.set("a", "x");
      inner.set("b", "y");
      outer.set("k", "v");
      root.delete("outer");
    },
  },
  {
    name: "map_in_array",
    clientID: 24,
    build: (d) => {
      const root = d.getArray("arr");
      const m = new Y.Map();
      root.insert(0, [m]);
      m.set("a", "x");
      m.set("b", "y");
      root.delete(0, 1);
    },
  },
];

const out = {
  generator: "yjs@13.6.32 (nested-type GC, V1)",
  scenarios: scenarios.map((s) => {
    const d = new Y.Doc({ gc: true });
    d.clientID = s.clientID;
    s.build(d);
    return {
      name: s.name,
      client_id: s.clientID,
      update_hex: toHex(Y.encodeStateAsUpdate(d)),
    };
  }),
};

const outPath = new URL("../nested-gc-fixtures.json", import.meta.url);
writeFileSync(outPath, JSON.stringify(out, null, 2) + "\n");
console.log(`wrote ${out.scenarios.length} nested-gc scenarios to ${outPath.pathname}`);
