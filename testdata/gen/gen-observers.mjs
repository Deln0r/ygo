// Generates testdata/observer-fixtures.json - which observers yjs@13.6.33
// calls for a transaction, in what order, and with which events.
//
// Each scenario builds a document in a setup transaction, registers shallow
// and deep observers (and optionally an afterTransaction handler), then runs
// one or more transactions of edits. Every observer call is logged as a line:
// "observe <name>" for a shallow observer, "deep <name> [<kind><path> ...]"
// for a deep one (one entry per event it received, in the order received), and
// "afterTransaction". The Go test (observer_fixture_test.go) replays the same
// edits through ygo's public API and compares the log line by line.
//
// Only names, kinds and paths are logged, not values: the event payloads are
// covered by the Map, Array and Text event tests.
//
// Run: node gen-observers.mjs (after npm install in this directory).

import { writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";
import * as Y from "yjs";

const here = dirname(fileURLToPath(import.meta.url));
const outPath = resolve(here, "..", "observer-fixtures.json");

// Ops. `path` locates the shared type the op acts on: a root map name, then
// map keys and array indices.
const setMap = (path, key) => ({ op: "setMap", path, key });
const setArray = (path, key) => ({ op: "setArray", path, key });
const setText = (path, key) => ({ op: "setText", path, key });
const set = (path, key, value) => ({ op: "set", path, key, value });
const del = (path, key) => ({ op: "delete", path, key });
const insertMap = (path, index) => ({ op: "insertMap", path, index });
const push = (path, value) => ({ op: "push", path, value });
const insert = (path, index, value) => ({ op: "insert", path, index, value });
const textInsert = (path, index, text) => ({ op: "textInsert", path, index, text });

const observe = (path, name) => ({ kind: "observe", path, name });
const deep = (path, name) => ({ kind: "deep", path, name });

const R = ["root"];
const at = (...rest) => [...R, ...rest];

const scenarios = [
  {
    name: "deep-paths-multiple-deep-observers",
    description: "yjs testDeepEventPathsWithMultipleDeepObservers (#768): deep observers at two levels",
    setup: [setMap(["a"], "b"), setMap(["a"], "c")],
    observers: [deep(["a"], "a"), deep(["a", "c"], "c"), observe(["a", "c"], "c")],
    afterTransaction: true,
    txns: [[set(["a", "b"], "foo", "bar"), set(["a", "c"], "foo", "bar")]],
  },
  {
    name: "new-type-written-in-same-transaction",
    description: "a nested map created and written in one transaction reports only through its parent",
    setup: [],
    observers: [observe(R, "root"), deep(R, "root")],
    txns: [[setMap(R, "child"), set(at("child"), "k", "v")]],
  },
  {
    name: "type-written-then-deleted",
    description: "a nested map written and then deleted in one transaction fires nothing of its own",
    setup: [setMap(R, "child")],
    observers: [observe(R, "root"), observe(at("child"), "child"), deep(R, "root")],
    txns: [[set(at("child"), "k", "v"), del(R, "child")]],
  },
  {
    name: "siblings-in-first-change-order",
    description: "observers of three siblings fire in the order the siblings were first changed",
    setup: [setMap(R, "z"), setMap(R, "a"), setMap(R, "m")],
    observers: [observe(at("z"), "z"), observe(at("a"), "a"), observe(at("m"), "m"), deep(R, "root")],
    txns: [[set(at("z"), "k", 1), set(at("a"), "k", 1), set(at("m"), "k", 1)]],
  },
  {
    name: "siblings-in-reverse-change-order",
    description: "the same siblings changed in the opposite order",
    setup: [setMap(R, "z"), setMap(R, "a"), setMap(R, "m")],
    observers: [observe(at("z"), "z"), observe(at("a"), "a"), observe(at("m"), "m"), deep(R, "root")],
    txns: [[set(at("m"), "k", 1), set(at("a"), "k", 1), set(at("z"), "k", 1)]],
  },
  {
    name: "deep-events-sorted-by-path-length",
    description: "a deep observer gets shallower events first, whatever order the changes happened in",
    setup: [setMap(R, "child"), setMap(at("child"), "grand")],
    observers: [deep(R, "root")],
    txns: [[set(at("child", "grand"), "k", 1), set(R, "x", 1)]],
  },
  {
    name: "array-index-path",
    description: "a map inside an array element is reached through its index",
    setup: [setArray(R, "list"), push(at("list"), "a"), insertMap(at("list"), 1)],
    observers: [deep(R, "root")],
    txns: [[set(at("list", 1), "k", "v")]],
  },
  {
    name: "text-in-map",
    description: "a text nested in a map fires its own observer and its parent's deep observer",
    setup: [setText(R, "t")],
    observers: [observe(at("t"), "t"), deep(R, "root")],
    txns: [[textInsert(at("t"), 0, "hi")]],
  },
  {
    name: "one-deep-call-per-transaction",
    description: "two transactions, one deep call each",
    setup: [setMap(R, "child")],
    observers: [deep(R, "root")],
    afterTransaction: true,
    txns: [[set(at("child"), "k", 1)], [set(at("child"), "k", 2)]],
  },
  {
    name: "grandchild-written-then-child-deleted",
    description: "deleting a nested map also deletes what is inside it, so the grandchild's change is dropped",
    setup: [setMap(R, "child"), setMap(at("child"), "grand")],
    observers: [observe(at("child", "grand"), "grand"), deep(R, "root")],
    txns: [[set(at("child", "grand"), "k", 1), del(R, "child")]],
  },
  {
    name: "only-changed-types-fire",
    description: "a change to the root map does not call observers of its children",
    setup: [setMap(R, "child")],
    observers: [observe(at("child"), "child"), observe(R, "root")],
    txns: [[set(R, "x", 1)]],
  },
  {
    name: "new-map-in-array-written",
    description: "a map inserted into an array and written in one transaction reports only through the array",
    setup: [setArray(R, "list")],
    observers: [observe(at("list"), "list"), deep(R, "root")],
    txns: [[insertMap(at("list"), 0), set(at("list", 0), "k", "v")]],
  },
  {
    name: "array-and-element-changed",
    description: "an array and a map inside it both change; shallow observers in first-change order, one deep call",
    setup: [setArray(R, "list"), insertMap(at("list"), 0)],
    observers: [observe(at("list"), "list"), observe(at("list", 0), "item"), deep(at("list"), "list")],
    txns: [[set(at("list", 0), "k", 1), push(at("list"), "x")]],
  },
  {
    name: "revisited-type-keeps-first-position",
    description: "a type changed again later in the transaction keeps the position of its first change",
    setup: [setMap(R, "z"), setMap(R, "a")],
    observers: [observe(at("z"), "z"), observe(at("a"), "a"), deep(R, "root")],
    txns: [[set(at("z"), "k", 1), set(at("a"), "k", 1), set(at("z"), "k", 2)]],
  },
  {
    name: "two-roots-in-change-order",
    description: "observers of two root maps fire in the order the roots were first changed",
    setup: [],
    observers: [observe(["a"], "a"), observe(["b"], "b")],
    txns: [[set(["b"], "k", 1), set(["a"], "k", 1)]],
  },
  {
    name: "array-index-after-shift",
    description: "the path of a map inside an array is its index after the whole transaction",
    setup: [setArray(R, "list"), insertMap(at("list"), 0), insertMap(at("list"), 1)],
    observers: [deep(R, "root")],
    txns: [[set(at("list", 1), "k", 1), insert(at("list"), 0, "x")]],
  },
  {
    name: "remote-new-type-written",
    description: "from a peer: a nested map created and written in one update reports only through its parent",
    remote: true,
    setup: [],
    observers: [observe(R, "root"), deep(R, "root")],
    afterTransaction: true,
    txns: [[setMap(R, "child"), set(at("child"), "k", "v")]],
  },
  {
    name: "remote-siblings",
    description: "from a peer: three siblings changed in one update, one deep call",
    remote: true,
    setup: [setMap(R, "z"), setMap(R, "a"), setMap(R, "m")],
    observers: [observe(at("z"), "z"), observe(at("a"), "a"), observe(at("m"), "m"), deep(R, "root")],
    txns: [[set(at("z"), "k", 1), set(at("a"), "k", 1), set(at("m"), "k", 1)]],
  },
  {
    name: "remote-type-written-then-deleted",
    description: "from a peer: a nested map written and deleted in one update fires nothing of its own",
    remote: true,
    setup: [setMap(R, "child")],
    observers: [observe(R, "root"), observe(at("child"), "child"), deep(R, "root")],
    txns: [[set(at("child"), "k", "v"), del(R, "child")]],
  },
];

const resolveType = (doc, path) => {
  let cur = doc.getMap(path[0]);
  for (const seg of path.slice(1)) {
    cur = cur.get(seg);
    if (cur === undefined) throw new Error(`no type at ${JSON.stringify(path)}`);
  }
  return cur;
};

const apply = (doc, op) => {
  const t = resolveType(doc, op.path);
  switch (op.op) {
    case "setMap": return t.set(op.key, new Y.Map());
    case "setArray": return t.set(op.key, new Y.Array());
    case "setText": return t.set(op.key, new Y.Text());
    case "set": return t.set(op.key, op.value);
    case "delete": return t.delete(op.key);
    case "insertMap": return t.insert(op.index, [new Y.Map()]);
    case "push": return t.push([op.value]);
    case "insert": return t.insert(op.index, [op.value]);
    case "textInsert": return t.insert(op.index, op.text);
    default: throw new Error(`unknown op ${op.op}`);
  }
};

const kind = (e) => (e.target instanceof Y.Map ? "map" : e.target instanceof Y.Text ? "text" : "array");
const pathStr = (p) => `[${p.join(" ")}]`;

// A remote scenario runs the edits on a second document (client 2) and
// applies each transaction to the observed one as an update, so the
// observers see what a peer's edit looks like.
const out = scenarios.map((sc) => {
  const doc = new Y.Doc();
  doc.clientID = 1;
  let src = null;
  if (sc.remote) {
    src = new Y.Doc();
    src.clientID = 2;
    src.transact(() => sc.setup.forEach((op) => apply(src, op)));
    Y.applyUpdate(doc, Y.encodeStateAsUpdate(src));
  } else {
    doc.transact(() => sc.setup.forEach((op) => apply(doc, op)));
  }
  const calls = [];
  for (const o of sc.observers) {
    const t = resolveType(doc, o.path);
    if (o.kind === "observe") {
      t.observe(() => calls.push(`observe ${o.name}`));
    } else {
      t.observeDeep((events) => calls.push(`deep ${o.name} [${events.map((e) => kind(e) + pathStr(e.path)).join(" ")}]`));
    }
  }
  if (sc.afterTransaction) doc.on("afterTransaction", () => calls.push("afterTransaction"));
  for (const txn of sc.txns) {
    if (src) {
      src.transact(() => txn.forEach((op) => apply(src, op)));
      Y.applyUpdate(doc, Y.encodeStateAsUpdate(src, Y.encodeStateVector(doc)));
    } else {
      doc.transact(() => txn.forEach((op) => apply(doc, op)));
    }
  }
  return {
    name: sc.name,
    description: sc.description,
    remote: !!sc.remote,
    setup: sc.setup,
    observers: sc.observers,
    after_transaction: !!sc.afterTransaction,
    txns: sc.txns,
    expected_calls: calls,
  };
});

const names = new Set();
for (const sc of out) {
  if (names.has(sc.name)) throw new Error(`duplicate scenario name ${sc.name}`);
  names.add(sc.name);
}

writeFileSync(
  outPath,
  JSON.stringify({ generator: "gen-observers.mjs (yjs@13.6.33)", scenarios: out }, null, 2) + "\n",
);
console.log(`wrote ${out.length} scenarios to ${outPath}`);
