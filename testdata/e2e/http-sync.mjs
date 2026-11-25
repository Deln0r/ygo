// Syncs yjs documents with a ygo server over HTTP (HTTPSyncHandler) with
// plain fetch, next to a y-websocket client of the same document, and
// exits non-zero if any check fails.
//
//   node http-sync.mjs path/to/hpserver
//
// It checks that what the server sends is what yjs itself would send
// (Y.encodeStateAsUpdate(V2) of a document holding the same updates, byte
// for byte), that HTTP writes reach a live WebSocket client and the other
// way round, and that an unchanged document answers a poll with 304.
import { spawn } from 'node:child_process'
import WebSocket from 'ws'
import * as Y from 'yjs'
import { WebsocketProvider } from 'y-websocket'

const hpserver = process.argv[2]
if (!hpserver) {
  console.error('usage: node http-sync.mjs path/to/hpserver')
  process.exit(2)
}

const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms))
async function waitFor (cond, ms = 5000) {
  const start = Date.now()
  while (Date.now() - start < ms) {
    if (cond()) return true
    await sleep(10)
  }
  return false
}

let failures = 0
function check (name, ok, detail = '') {
  console.log(`${ok ? 'PASS' : 'FAIL'} ${name}${ok || !detail ? '' : `: ${detail}`}`)
  if (!ok) failures++
}

const b64 = (u8) => Buffer.from(u8).toString('base64')
const hex = (u8) => Buffer.from(u8).toString('hex')

const proc = spawn(hpserver, ['-no-auth'], { stdio: ['ignore', 'pipe', 'inherit'] })
const wsURL = await new Promise((resolve, reject) => {
  proc.stdout.once('data', (d) => resolve(String(d).trim()))
  proc.once('exit', (code) => reject(new Error(`hpserver exited ${code}`)))
})
const httpURL = wsURL.replace('ws://', 'http://') + '/sync/'

async function get (room, sv, { v2 = false, etag } = {}) {
  const q = new URLSearchParams()
  if (sv) q.set('sv', b64(sv))
  if (v2) q.set('v', '2')
  const res = await fetch(`${httpURL}${room}?${q}`, { headers: etag ? { 'If-None-Match': etag } : {} })
  return { status: res.status, etag: res.headers.get('etag'), sv: res.headers.get('ygo-state-vector'), body: new Uint8Array(await res.arrayBuffer()) }
}

async function post (room, update, sv) {
  const q = sv ? `?sv=${encodeURIComponent(b64(sv))}` : ''
  const res = await fetch(`${httpURL}${room}${q}`, { method: 'POST', headers: { 'Content-Type': 'application/octet-stream' }, body: update })
  return { status: res.status, body: new Uint8Array(await res.arrayBuffer()) }
}

try {
  // The mirror holds every update the server got, as yjs would.
  const mirror = new Y.Doc()
  const a = new Y.Doc()
  const updatesOfA = []
  a.on('update', (u) => { updatesOfA.push(u); Y.applyUpdate(mirror, u) })
  a.getText('t').insert(0, 'hello from fetch')
  a.getMap('m').set('k', { nested: [1, 'two', null] })
  for (const u of updatesOfA) {
    const r = await post('room', u)
    check('POST an update', r.status === 204, `status ${r.status}`)
  }

  // A y-websocket client of the same document.
  const live = new Y.Doc()
  const provider = new WebsocketProvider(wsURL, 'room', live, { WebSocketPolyfill: WebSocket, disableBc: true })
  check('the WebSocket client gets what was POSTed', await waitFor(() => live.getText('t').toString() === 'hello from fetch'))
  live.on('update', (u, origin) => { if (origin !== provider) Y.applyUpdate(mirror, u) })
  live.getText('t').insert(5, ',')

  // Poll until the WebSocket client's edit has reached the server.
  let b
  let r
  for (let i = 0; i < 100; i++) {
    b = new Y.Doc()
    r = await get('room', Y.encodeStateVector(b))
    Y.applyUpdate(b, r.body)
    if (b.getText('t').toString() === 'hello, from fetch') break
    await sleep(20)
  }
  check('GET 200', r.status === 200, `status ${r.status}`)
  check('GET brings the WebSocket client\'s edit', b.getText('t').toString() === 'hello, from fetch', b.getText('t').toString())
  check('GET is yjs\'s encodeStateAsUpdate, byte for byte', hex(r.body) === hex(Y.encodeStateAsUpdate(mirror, Y.encodeStateVector(new Y.Doc()))), `${hex(r.body)} vs ${hex(Y.encodeStateAsUpdate(mirror))}`)
  check('the state vector header is yjs\'s', r.sv === b64(Y.encodeStateVector(mirror)), `${r.sv} vs ${b64(Y.encodeStateVector(mirror))}`)

  // Against a state vector inside the document, V1 and V2.
  const partial = Y.encodeStateVector(a)
  r = await get('room', partial)
  check('GET at a state vector is yjs\'s diff', hex(r.body) === hex(Y.encodeStateAsUpdate(mirror, partial)))
  const r2 = await get('room', partial, { v2: true })
  check('GET v=2 is yjs\'s V2 diff', hex(r2.body) === hex(Y.encodeStateAsUpdateV2(mirror, partial)))

  // Polling.
  const tag = r.etag
  const same = await get('room', Y.encodeStateVector(b), { etag: tag })
  check('an unchanged document answers 304', same.status === 304 && same.body.length === 0, `status ${same.status}`)
  live.getText('t').insert(0, '>')
  let changed
  for (let i = 0; i < 100; i++) {
    changed = await get('room', Y.encodeStateVector(b), { etag: tag })
    if (changed.status !== 304) break
    await sleep(20)
  }
  check('a changed document answers 200', changed.status === 200, `status ${changed.status}`)
  Y.applyUpdate(b, changed.body)
  check('and brings the change', b.getText('t').toString() === '>hello, from fetch', b.getText('t').toString())

  // Push and pull in one request.
  const c = new Y.Doc()
  c.getArray('a').push(['from c'])
  const both = await post('room', Y.encodeStateAsUpdate(c), Y.encodeStateVector(c))
  check('POST with sv answers 200', both.status === 200, `status ${both.status}`)
  Y.applyUpdate(c, both.body)
  check('and brings the rest of the document', c.getText('t').toString() === '>hello, from fetch')
  check('the WebSocket client gets the POST', await waitFor(() => live.getArray('a').toArray().join() === 'from c'))

  // A deletion only.
  const before = Y.encodeStateVector(c)
  c.getText('t').delete(0, 1)
  const del = Y.encodeStateAsUpdate(c, before)
  check('POST a deletion', (await post('room', del)).status === 204)
  check('the WebSocket client gets the deletion', await waitFor(() => live.getText('t').toString() === 'hello, from fetch'))

  provider.destroy()
} catch (e) {
  console.error(e)
  failures++
} finally {
  proc.kill('SIGTERM')
}
if (failures) {
  console.error(`${failures} check(s) failed`)
  process.exit(1)
}
console.log('all checks passed')
// fetch keeps idle connections, which would keep the process alive.
process.exit(0)
