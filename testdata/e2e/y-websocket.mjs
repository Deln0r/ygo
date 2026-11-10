// Runs stock y-websocket clients (2.1 and 3.1) against a yserve binary and
// exits non-zero if any check fails.
//
//   node y-websocket.mjs path/to/yserve
//
// The fixtures check bytes; this checks what a JavaScript user sees. It
// exists because two bugs lived for months behind passing fixtures: rooms
// whose names differ before the first slash shared one document, and the
// default read limit dropped any message over 32 KiB.
import { spawn } from 'node:child_process'
import { mkdtempSync, rmSync } from 'node:fs'
import { createServer } from 'node:net'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import WebSocket from 'ws'
import * as Y from 'yjs'
import { WebsocketProvider as Provider3 } from 'y-websocket'
import { WebsocketProvider as Provider2 } from 'y-websocket-2'

const yserve = process.argv[2]
if (!yserve) {
  console.error('usage: node y-websocket.mjs path/to/yserve')
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

const freePort = () => new Promise((resolve, reject) => {
  const s = createServer()
  s.on('error', reject)
  s.listen(0, '127.0.0.1', () => {
    const { port } = s.address()
    s.close(() => resolve(port))
  })
})

async function startServer (args) {
  const port = await freePort()
  const proc = spawn(yserve, ['-addr', `127.0.0.1:${port}`, ...args], { stdio: ['ignore', 'ignore', 'pipe'] })
  let log = ''
  proc.stderr.on('data', (d) => { log += d })
  if (!await waitFor(() => log.includes('listening on'), 10000)) {
    proc.kill()
    throw new Error(`yserve did not start:\n${log}`)
  }
  const stop = () => new Promise((resolve) => {
    proc.on('exit', resolve)
    proc.kill('SIGTERM')
  })
  return { url: `ws://127.0.0.1:${port}`, stop }
}

let failures = 0
function check (name, ok) {
  console.log(`${ok ? 'PASS' : 'FAIL'} ${name}`)
  if (!ok) failures++
}

const providers = { 'y-websocket 2.1': Provider2, 'y-websocket 3.1': Provider3 }
const open = []

function connect (Provider, url, room) {
  const doc = new Y.Doc()
  const provider = new Provider(url, room, doc, { WebSocketPolyfill: WebSocket, disableBc: true })
  const closes = []
  provider.on('connection-close', (e) => closes.push(e && e.code))
  const client = { doc, provider, closes, close: () => { provider.destroy(); doc.destroy() } }
  open.push(client)
  return client
}

const synced = (c) => waitFor(() => c.provider.synced)
const text = (c) => c.doc.getText('t').toString()

const dir = mkdtempSync(join(tmpdir(), 'ygo-e2e-'))
const store = join(dir, 'e2e.db')

try {
  let server = await startServer(['-store', store])
  for (const [name, Provider] of Object.entries(providers)) {
    const room = `room-${name.replace(/\W/g, '')}`
    const a = connect(Provider, server.url, room)
    const b = connect(Provider, server.url, room)
    check(`${name}: both clients sync`, await synced(a) && await synced(b))

    a.doc.getText('t').insert(0, 'from a')
    check(`${name}: an edit reaches the other client`, await waitFor(() => text(b) === 'from a'))

    b.provider.awareness.setLocalStateField('user', { name: 'b' })
    check(`${name}: presence reaches the other client`,
      await waitFor(() => a.provider.awareness.getStates().get(b.doc.clientID)?.user?.name === 'b'))

    const paste = 'x'.repeat(200000)
    b.doc.getText('t').insert(0, paste)
    check(`${name}: a 200 000-character paste reaches the other client`,
      await waitFor(() => text(a).length === paste.length + 6))
    check(`${name}: the paste closed no connection`, a.closes.length === 0 && b.closes.length === 0)

    const bID = b.doc.clientID
    b.close()
    check(`${name}: presence goes when a client leaves`,
      await waitFor(() => !a.provider.awareness.getStates().has(bID), 8000))

    const late = connect(Provider, server.url, room)
    check(`${name}: a late joiner gets the document from the server`,
      await waitFor(() => text(late).length === paste.length + 6))

    const x = connect(Provider, server.url, `team-a/doc-${room}`)
    const y = connect(Provider, server.url, `team-b/doc-${room}`)
    await synced(x)
    await synced(y)
    const x2 = connect(Provider, server.url, `team-a/doc-${room}`)
    await synced(x2)
    x.doc.getText('t').insert(0, 'team a only')
    // Once the edit has reached a peer in its own room, the server has
    // applied and broadcast it, so the other room would have it too if the
    // two were one document.
    check(`${name}: the same room with a slash converges`, await waitFor(() => text(x2) === 'team a only'))
    await sleep(100)
    check(`${name}: rooms that differ before a slash stay apart`, text(y) === '')
  }

  while (open.length) open.pop().close()
  await server.stop()
  server = await startServer(['-store', store])
  const back = connect(Provider3, server.url, 'room-ywebsocket31')
  check('after a restart the stored document comes back', await waitFor(() => text(back).length === 200006))
  while (open.length) open.pop().close()
  await server.stop()

  // The prefix is not part of the name: a document stored without a
  // prefix is the same document under -path-prefix.
  server = await startServer(['-store', store, '-path-prefix', '/ws/'])
  const stored = connect(Provider3, `${server.url}/ws`, 'team-a/doc-room-ywebsocket31')
  check('-path-prefix: the prefix is not part of the document name', await waitFor(() => text(stored) === 'team a only'))
  const p = connect(Provider3, `${server.url}/ws`, 'team-a/doc')
  const q = connect(Provider3, `${server.url}/ws`, 'team-a/doc')
  const r = connect(Provider3, `${server.url}/ws`, 'team-b/doc')
  await synced(p)
  await synced(q)
  await synced(r)
  p.doc.getText('t').insert(0, 'under /ws/')
  check('-path-prefix: the same room converges', await waitFor(() => text(q) === 'under /ws/'))
  check('-path-prefix: another room stays apart', text(r) === '')
  while (open.length) open.pop().close()
  await server.stop()
} finally {
  while (open.length) open.pop().close()
  rmSync(dir, { recursive: true, force: true })
}

if (failures) {
  console.error(`${failures} check(s) failed`)
  process.exit(1)
}
console.log('all checks passed')
process.exit(0)
