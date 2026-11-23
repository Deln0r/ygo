// The yjs interpreter of the differential fuzzer (internal/difffuzz): run(s)
// replays a scenario through yjs and returns its trace, in the schema
// internal/difffuzz/run.go writes; the code below follows that file op for
// op. Used by difffuzz-oracle.mjs (the worker the Go tests drive) and
// gen-difffuzz.mjs (the corpus fixtures).

import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import * as Y from "yjs";

export const SCHEMA = 1;
const here = dirname(fileURLToPath(import.meta.url));
for (const [pkg, want] of [["yjs", "13.6.33"], ["lib0", "0.2.119"]]) {
  const got = JSON.parse(readFileSync(resolve(here, "node_modules", pkg, "package.json"), "utf8")).version;
  if (got !== want) {
    process.stderr.write(`difffuzz-oracle: ${pkg} ${got}, want ${want}\n`);
    process.exit(2);
  }
}

const toHex = (u8) => Buffer.from(u8).toString("hex");
const sum = (u8) => createHash("sha256").update(u8).digest("hex").slice(0, 16);

// Tagged values (internal/difffuzz/value.go).
function value(t) {
  switch (t[0]) {
    case "n": return null;
    case "i": case "f": case "s": case "b": return t[1];
    case "a": return t[1].map(value);
    case "o": {
      const o = {};
      for (const [k, v] of t[1]) o[k] = value(v);
      return o;
    }
  }
  throw new Error(`unknown tag ${t[0]}`);
}
const attrs = (kv) => {
  if (kv === undefined || kv === null) return undefined;
  const o = {};
  for (const { k, v } of kv) o[k] = value(v);
  return o;
};

function mulberry32(seed) {
  let a = seed >>> 0;
  return () => {
    a = (a + 0x6d2b79f5) >>> 0;
    let t = Math.imul(a ^ (a >>> 15), a | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return (t ^ (t >>> 14)) >>> 0;
  };
}

// A value in an event: shared types by kind only (run.go eventValue).
function eventValue(v) {
  let kind = null;
  if (v instanceof Y.Map) kind = "$map";
  else if (v instanceof Y.Array) kind = "$array";
  else if (v instanceof Y.XmlText) kind = "$xt";
  else if (v instanceof Y.Text) kind = "$text";
  else if (v instanceof Y.XmlFragment) kind = "$xml";
  if (kind !== null) return v._item ? `${kind}@${v._item.id.client}:${v._item.id.clock}` : kind;
  if (v === undefined) return null;
  return canon(v);
}

// The view: the same tagged tree as run.go's view, objects rebuilt with
// their keys sorted (array-index keys come first in JavaScript anyway), so
// JSON.stringify writes what anyval.AppendStringify writes.
// Keys in code point order, as Go's sort.Strings orders UTF-8; objects
// without a prototype, so a "__proto__" key is a key.
const byCodePoint = (a, b) => {
  const A = Array.from(a), B = Array.from(b);
  for (let i = 0; i < Math.min(A.length, B.length); i++) {
    const x = A[i].codePointAt(0), y = B[i].codePointAt(0);
    if (x !== y) return x - y;
  }
  return A.length - B.length;
};
function canon(v) {
  if (Array.isArray(v)) return v.map(canon);
  if (v !== null && typeof v === "object" && !(v instanceof Y.AbstractType) && !(v instanceof Uint8Array)) {
    const o = Object.create(null);
    for (const k of Object.keys(v).sort(byCodePoint)) o[k] = canon(v[k]);
    return o;
  }
  return valueView(v);
}
function valueView(v) {
  if (v instanceof Y.Map) return { $map: mapView(v) };
  if (v instanceof Y.Array) return { $array: v.toArray().map(valueView) };
  if (v instanceof Y.XmlText) return { $xt: textView(v) };
  if (v instanceof Y.Text) return { $text: textView(v) };
  if (v instanceof Y.XmlElement || v instanceof Y.XmlFragment) return xmlView(v);
  if (v !== null && typeof v === "object") return canon(v);
  return v;
}
function mapView(m) {
  const o = Object.create(null);
  for (const k of Array.from(m.keys()).sort(byCodePoint)) o[k] = valueView(m.get(k));
  return o;
}
function textView(t) {
  return t.toDelta().map((d) => {
    const op = { insert: typeof d.insert === "string" ? d.insert : valueView(d.insert) };
    if (d.attributes && Object.keys(d.attributes).length > 0) op.attributes = canon(d.attributes);
    return canon(op);
  });
}
function xmlView(n) {
  const children = n.toArray().map(valueView);
  if (n instanceof Y.XmlElement) return canon({ $el: n.nodeName, attrs: n.getAttributes(), ch: children });
  return { $xml: children };
}

function resolveTarget(p, op) {
  let cur = { t: p.t, a: p.a, m: p.m, x: p.x }[op.root];
  if (cur === undefined) return null;
  for (const seg of op.path || []) {
    const hint = seg.length > 1 && seg[0] === "#" ? Number(seg.slice(1)) : -1;
    if (cur instanceof Y.Map) {
      if (hint >= 0) return null;
      cur = cur.get(seg);
    } else if (cur instanceof Y.Array || cur instanceof Y.XmlFragment) {
      if (hint < 0 || cur.length === 0) return null;
      cur = cur.get(hint % cur.length);
    } else return null;
  }
  switch (op.want) {
    case "text": return cur instanceof Y.Text && !(cur instanceof Y.XmlText) ? cur : null;
    case "xmltext": return cur instanceof Y.XmlText ? cur : null;
    case "array": return cur instanceof Y.Array ? cur : null;
    case "map": return cur instanceof Y.Map ? cur : null;
    case "xml": return cur instanceof Y.XmlFragment ? cur : null;
    case "xmlel": return cur instanceof Y.XmlElement ? cur : null;
  }
  return null;
}

const insIdx = (h, l) => (h || 0) % (l + 1);
const delRange = (h, n, l) => {
  if (l === 0) return null;
  const idx = (h || 0) % l;
  return [idx, Math.min(Math.max(n || 0, 1), l - idx)];
};
const utf16Len = (s) => s.length;

function resolveDelta(steps, l) {
  const out = [];
  let c = 0;
  for (const d of steps) {
    const a = attrs(d.a);
    if (d.r !== undefined) {
      if (l - c === 0) continue;
      const n = 1 + (d.r % (l - c));
      out.push(a === undefined ? { retain: n } : { retain: n, attributes: a });
      c += n;
    } else if (d.d !== undefined) {
      if (l - c === 0) continue;
      const n = 1 + (d.d % (l - c));
      out.push({ delete: n });
      l -= n;
    } else if (d.i !== undefined) {
      out.push(a === undefined ? { insert: d.i } : { insert: d.i, attributes: a });
      c += utf16Len(d.i);
      l += utf16Len(d.i);
    } else if (d.e !== undefined) {
      const e = value(d.e);
      out.push(a === undefined ? { insert: e } : { insert: e, attributes: a });
      c++;
      l++;
    }
  }
  return out;
}

function textOp(t, op) {
  const l = t.length;
  const a = attrs(op.attrs);
  switch (op.o) {
    case "ins":
      if (a === undefined) t.insert(insIdx(op.pos, l), op.s ?? "");
      else t.insert(insIdx(op.pos, l), op.s ?? "", a);
      return;
    case "del": {
      const r = delRange(op.pos, op.len, l);
      if (r) t.delete(r[0], r[1]);
      return;
    }
    case "fmt": {
      const r = delRange(op.pos, op.len, l);
      if (r) t.format(r[0], r[1], a);
      return;
    }
    case "emb":
      if (a === undefined) t.insertEmbed(insIdx(op.pos, l), value(op.v));
      else t.insertEmbed(insIdx(op.pos, l), value(op.v), a);
      return;
    case "delta": {
      const ops = resolveDelta(op.delta || [], l);
      if (ops.length > 0) t.applyDelta(ops);
      return;
    }
  }
  throw new Error(`text op ${op.o}`);
}

function runOp(p, op) {
  const t = resolveTarget(p, op);
  if (t === null || t === undefined) return;
  if (t instanceof Y.Text) return textOp(t, op);
  if (t instanceof Y.Array) {
    const l = t.length;
    switch (op.o) {
      case "ins": t.insert(insIdx(op.pos, l), value(op.v)); return;
      case "push": t.push(value(op.v)); return;
      case "del": { const r = delRange(op.pos, op.len, l); if (r) t.delete(r[0], r[1]); return; }
      case "insmap": t.insert(insIdx(op.pos, l), [new Y.Map()]); return;
      case "insarr": t.insert(insIdx(op.pos, l), [new Y.Array()]); return;
      case "instext": t.insert(insIdx(op.pos, l), [new Y.Text()]); return;
    }
    throw new Error(`array op ${op.o}`);
  }
  if (t instanceof Y.Map) {
    switch (op.o) {
      case "set": t.set(op.key ?? "", value(op.v)); return;
      case "mdel": t.delete(op.key ?? ""); return;
      case "setmap": t.set(op.key ?? "", new Y.Map()); return;
      case "setarr": t.set(op.key ?? "", new Y.Array()); return;
      case "settext": t.set(op.key ?? "", new Y.Text()); return;
      case "clear": t.clear(); return;
    }
    throw new Error(`map op ${op.o}`);
  }
  if (t instanceof Y.XmlFragment) {
    if (t instanceof Y.XmlElement && op.o === "setattr") { t.setAttribute(op.key ?? "", op.s ?? ""); return; }
    if (t instanceof Y.XmlElement && op.o === "rmattr") { t.removeAttribute(op.key ?? ""); return; }
    const l = t.length;
    switch (op.o) {
      case "insel": t.insert(insIdx(op.pos, l), [new Y.XmlElement(op.s ?? "")]); return;
      case "instext": t.insert(insIdx(op.pos, l), [new Y.XmlText()]); return;
      case "del": { const r = delRange(op.pos, op.len, l); if (r) t.delete(r[0], r[1]); return; }
    }
    throw new Error(`xml op ${op.o}`);
  }
}

export function run(s) {
  let cur = null;
  const peers = s.peers.map((ps, i) => {
    const doc = new Y.Doc({ gc: ps.gc });
    doc.clientID = ps.client;
    const p = { doc, t: doc.getText("t"), a: doc.getArray("a"), m: doc.getMap("m"), x: doc.getXmlFragment("x"), log: [], sent: s.peers.map(() => new Set()), um: null };
    if (ps.undo) p.um = new Y.UndoManager([p.t, p.a, p.m, p.x], { captureTimeout: Infinity });
    doc.on("update", (u) => {
      const b = new Uint8Array(u);
      p.log.push({ v1: b, v2: null });
      if (cur) cur.events.push({ peer: i, v: "1", hex: toHex(b) });
    });
    doc.on("updateV2", (u) => {
      const b = new Uint8Array(u);
      const l = p.log;
      if (l.length > 0 && l[l.length - 1].v2 === null) l[l.length - 1].v2 = b;
      if (cur) cur.events.push({ peer: i, v: "2", hex: toHex(b) });
    });
    if (s.observe) {
      const rec = (root, evs) => {
        if (!cur) return;
        cur.batch++;
        for (const e of evs) {
          const o = { batch: cur.batch, peer: i, root, path: JSON.stringify(e.path), kind: "", change: "" };
          if (e.target instanceof Y.Map) {
            o.kind = "map";
            const keys = Array.from(e.changes.keys.entries()).sort((a, b) => (a[0] < b[0] ? -1 : a[0] > b[0] ? 1 : 0));
            o.change = JSON.stringify(keys.map(([key, c]) => canon(c.action === "add" ? { key, action: c.action } : { key, action: c.action, old: eventValue(c.oldValue) })));
          } else if (e.target instanceof Y.Text) {
            o.kind = "text";
            o.change = JSON.stringify(e.delta.map((d) => {
              const op = {};
              if (d.retain !== undefined) op.retain = d.retain;
              else if (d.delete !== undefined) op.delete = d.delete;
              else op.insert = typeof d.insert === "string" ? d.insert : eventValue(d.insert);
              if (d.attributes && Object.keys(d.attributes).length > 0) op.attributes = d.attributes;
              return canon(op);
            }));
          } else if (e.target instanceof Y.Array) {
            o.kind = "array";
            o.change = JSON.stringify(e.changes.delta.map((d) => {
              if (d.insert !== undefined) return { insert: d.insert.map(eventValue) };
              if (d.delete !== undefined) return { delete: d.delete };
              return { retain: d.retain };
            }));
          } else o.kind = String(e.constructor && e.constructor.name);
          cur.obs.push(o);
        }
      };
      p.a.observeDeep((evs) => rec("a", evs));
      p.m.observeDeep((evs) => rec("m", evs));
      p.t.observe((e) => rec("t", [e]));
    }
    return p;
  });
  const apply = (doc, u, v2) => (v2 ? Y.applyUpdateV2(doc, u, "remote") : Y.applyUpdate(doc, u, "remote"));
  const undelivered = (from, to) => from.log.map((_, i) => i).filter((i) => !from.sent[to].has(i));
  const order = (idx, st) => {
    let out = idx.slice();
    switch (st.order) {
      case "rev": out.reverse(); break;
      case "shuf": case "part": {
        const rnd = mulberry32(st.r || 0);
        for (let i = out.length - 1; i > 0; i--) {
          const j = rnd() % (i + 1);
          [out[i], out[j]] = [out[j], out[i]];
        }
        if (st.order === "part") out = out.slice(0, 1 + ((st.n || 0) % out.length));
        break;
      }
      case "dup": out = out.flatMap((i) => [i, i]); break;
      case "late": if (out.length > 1) out = out.slice(1); break;
    }
    return out;
  };
  const state = (i) => {
    const p = peers[i];
    const ps = {
      peer: i,
      v1: sum(Y.encodeStateAsUpdate(p.doc)),
      v2: sum(Y.encodeStateAsUpdateV2(p.doc)),
      sv: toHex(Y.encodeStateVector(p.doc)),
      pending: p.doc.store.pendingStructs !== null || p.doc.store.pendingDs !== null,
      view: JSON.stringify(canon({ t: textView(p.t), a: p.a.toArray().map(valueView), m: mapView(p.m), x: xmlView(p.x) })),
    };
    if (p.um) {
      if (p.um.canUndo()) ps.canUndo = true;
      if (p.um.canRedo()) ps.canRedo = true;
    }
    return ps;
  };
  const steps = [];
  for (const st of s.steps) {
    const out = { events: [], obs: [], batch: 0 };
    cur = out;
    let touched = [];
    try {
      switch (st.k) {
        case "txn": {
          const p = peers[st.p || 0];
          try {
            p.doc.transact(() => { for (const op of st.ops || []) runOp(p, op); });
          } catch (e) {
            out.err = "error";
          }
          touched = [st.p || 0];
          break;
        }
        case "undo": case "redo": case "stop": case "uclear": {
          const p = peers[st.p || 0];
          if (!p.um) { out.skip = true; break; }
          if (st.k === "undo") out.result = p.um.undo() !== null;
          else if (st.k === "redo") out.result = p.um.redo() !== null;
          else if (st.k === "stop") p.um.stopCapturing();
          else p.um.clear();
          touched = [st.p || 0];
          break;
        }
        case "deliver": {
          const from = peers[st.from || 0], to = peers[st.to || 0];
          const idx = undelivered(from, st.to || 0);
          if (idx.length === 0) { out.skip = true; break; }
          try {
            for (const i of order(idx, st)) {
              from.sent[st.to || 0].add(i);
              apply(to.doc, st.v2 ? from.log[i].v2 : from.log[i].v1, st.v2);
            }
          } catch (e) { out.err = "error"; }
          touched = [st.to || 0];
          break;
        }
        case "sync": {
          const from = peers[st.from || 0], to = peers[st.to || 0];
          try {
            const sv = Y.encodeStateVector(to.doc);
            const u = st.v2 ? Y.encodeStateAsUpdateV2(from.doc, sv) : Y.encodeStateAsUpdate(from.doc, sv);
            out.sent = toHex(u);
            apply(to.doc, u, st.v2);
          } catch (e) { out.err = "error"; }
          touched = [st.to || 0];
          break;
        }
        case "merge": case "diff": {
          const from = peers[st.from || 0], to = peers[st.to || 0];
          const idx = undelivered(from, st.to || 0);
          if (idx.length === 0) { out.skip = true; break; }
          try {
            const ups = idx.map((i) => { from.sent[st.to || 0].add(i); return st.v2 ? from.log[i].v2 : from.log[i].v1; });
            let u = st.v2 ? Y.mergeUpdatesV2(ups) : Y.mergeUpdates(ups);
            if (st.k === "diff") {
              const sv = Y.encodeStateVector(to.doc);
              u = st.v2 ? Y.diffUpdateV2(u, sv) : Y.diffUpdate(u, sv);
            }
            out.sent = toHex(u);
            apply(to.doc, u, st.v2);
          } catch (e) { out.err = "error"; }
          touched = [st.to || 0];
          break;
        }
        case "final": {
          try {
            for (let round = 0; round < 10; round++) {
              let changed = false;
              for (let a = 0; a < peers.length; a++) {
                for (let b = 0; b < peers.length; b++) {
                  if (a === b) continue;
                  const sv = Y.encodeStateVector(peers[b].doc);
                  apply(peers[b].doc, Y.encodeStateAsUpdate(peers[a].doc, sv), false);
                  if (toHex(Y.encodeStateVector(peers[b].doc)) !== toHex(sv)) changed = true;
                }
              }
              if (!changed) break;
            }
          } catch (e) { out.err = "error"; break; }
          touched = peers.map((_, i) => i);
          break;
        }
        case "unwind": {
          out.depths = [];
          peers.forEach((p, i) => {
            if (!p.um) return;
            let u = 0, d = 0;
            while (u < 1000 && p.um.undo() !== null) u++;
            while (d < 1000 && p.um.redo() !== null) d++;
            out.depths.push(u, d);
            touched.push(i);
          });
          if (out.depths.length === 0) delete out.depths;
          break;
        }
        default:
          out.err = `unknown step ${st.k}`;
      }
    } catch (e) {
      out.err = `exception: ${e && e.message}`;
    }
    cur = null;
    if (!out.skip && !(out.err && out.err.startsWith("unknown"))) out.peers = touched.map(state);
    if (out.events.length === 0) delete out.events;
    delete out.batch;
    if (out.obs.length === 0) delete out.obs;
    steps.push(out);
  }
  return { schema: SCHEMA, steps };
}

