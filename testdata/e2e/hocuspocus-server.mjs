// Stock Hocuspocus providers 2/3/4 against ygo's server. Run with:
// node hocuspocus-server.mjs path/to/hpserver path/to/yserve
import assert from 'node:assert/strict'
import { spawn } from 'node:child_process'
import { createServer } from 'node:net'
import WebSocket from 'ws'
import * as Y from 'yjs'
import { WebsocketProvider } from 'y-websocket'

const [bin, yserve] = process.argv.slice(2)
if (!bin || !yserve) throw new Error('usage: hocuspocus-server.mjs hpserver yserve')
const sleep = ms => new Promise(resolve => setTimeout(resolve, ms))
async function waitFor (f, label, timeout = 5000) {
  const end = Date.now() + timeout
  while (Date.now() < end) { if (f()) { console.log('PASS ' + label); return }; await sleep(10) }
  throw new Error(label)
}
const packages = { 2: 'hocuspocus-provider-2', 3: 'hocuspocus-provider-3', 4: '@hocuspocus/provider' }
const clients = []
const servers = []
async function start (noAuth = false) {
  const p = spawn(bin, noAuth ? ['-no-auth'] : [], { stdio: ['ignore', 'pipe', 'inherit'] })
  servers.push(p)
  let url = ''
  p.stdout.on('data', data => { url += data.toString() })
  await waitFor(() => url.includes('\n'), 'server starts')
  return url.trim()
}
async function provider (v, url, name, token = 'ok', extra = {}) {
  const mod = await import(packages[v])
  const document = extra.document ?? new Y.Doc()
  const events = { auth: [], denied: [], stateless: [], closes: [] }
  const p = new mod.HocuspocusProvider({ url, name, document, token,
    WebSocketPolyfill: WebSocket, broadcast: false,
    preserveConnection: Boolean(extra.websocketProvider),
    onAuthenticated: e => events.auth.push(e?.scope ?? p.authorizedScope),
    onAuthenticationFailed: e => events.denied.push(e.reason),
    onStateless: e => events.stateless.push(e.payload),
    onClose: e => events.closes.push(e),
    ...extra })
  if (extra.websocketProvider && v !== 2) p.attach()
  const c = { p, doc: document, map: document.getMap('m'), events,
    close: () => { p.destroy(); document.destroy() } }
  clients.push(c)
  return c
}
const synced = c => c.p.isSynced || c.p.synced

try {
  const base = await start()
  const live = []
  for (const v of [2, 3, 4]) {
    const name = `provider-${v}`
    const d = new Y.Doc(); d.getMap('m').set('offline', 'before-connect')
    // Token resolution happens after the provider queues its sync frames.
    const a = await provider(v, base + '/collab/', name, async () => { await sleep(60); return 'ok' }, { document: d })
    await waitFor(() => synced(a) && !a.p.hasUnsyncedChanges, `hp${v}: offline sync and exact acknowledgement count`)
    assert.deepEqual(a.events.auth, ['read-write'])
    const b = await provider(v, base + '/collab', name)
    await waitFor(() => synced(b) && b.map.get('offline') === 'before-connect', `hp${v}: late joiner`)
    a.map.set('forward', 'a')
    await waitFor(() => b.map.get('forward') === 'a', `hp${v}: edit to peer`)
    b.map.set('back', 'b')
    await waitFor(() => a.map.get('back') === 'b' && !a.p.hasUnsyncedChanges && !b.p.hasUnsyncedChanges, `hp${v}: edit back and unsynced cleared`)
    a.p.setAwarenessField('user', 'alice')
    await waitFor(() => b.p.awareness.getStates().get(a.doc.clientID)?.user === 'alice', `hp${v}: awareness`)
    a.p.sendStateless('hello')
    await waitFor(() => a.events.stateless.includes('reply:hello') && b.events.stateless.includes('reply:hello'), `hp${v}: stateless broadcast`)

    const ro = await provider(v, base + '/collab', name, 'ro')
    await waitFor(() => synced(ro) && ro.events.auth.includes('readonly') && ro.map.get('forward') === 'a', `hp${v}: read-only initial sync`)
    ro.map.set('blocked', 'no')
    await sleep(100)
    assert.equal(a.map.has('blocked'), false)
    assert.equal(b.map.has('blocked'), false)
    console.log(`PASS hp${v}: read-only edit withheld from peers`)
    const denied = await provider(v, base + '/collab', name, 'bad')
    await waitFor(() => denied.events.denied.includes('bad-token'), `hp${v}: authentication denied`)
    assert.equal(denied.events.auth.length, 0)
    denied.close(); ro.close()

    const yd = new Y.Doc()
    const yw = new WebsocketProvider(base + '/collab', name, yd, { WebSocketPolyfill: WebSocket, disableBc: true })
    clients.push({ close: () => { yw.destroy(); yd.destroy() } })
    await waitFor(() => yw.synced && yd.getMap('m').get('back') === 'b', `hp${v}: y-websocket reads the same room`)
    yd.getMap('m').set('mixed', 'y-websocket')
    await waitFor(() => a.map.get('mixed') === 'y-websocket', `hp${v}: y-websocket edit reaches provider`)
    a.map.set('reverse', 'hocuspocus')
    await waitFor(() => yd.getMap('m').get('reverse') === 'hocuspocus', `hp${v}: provider edit reaches y-websocket`)
    live.push({ v, a, b, closes: a.events.closes.length })
  }

  for (const v of [2, 3, 4]) {
    const { HocuspocusProviderWebsocket } = await import(packages[v])
    const ws = new HocuspocusProviderWebsocket({ url: base + '/collab', WebSocketPolyfill: WebSocket })
    clients.push({ close: () => ws.destroy() })
    const a = await provider(v, base, `multiplex-a-${v}`, 'ok', { websocketProvider: ws })
    const b = await provider(v, base, `multiplex-b-${v}`, 'ok', { websocketProvider: ws })
    await waitFor(() => synced(a) && synced(b), `hp${v}: two documents on one socket`)
    assert.equal(a.p.configuration.websocketProvider, b.p.configuration.websocketProvider)
    a.map.set('private', 'a')
    await waitFor(() => !a.p.hasUnsyncedChanges, `hp${v}: multiplex edit acknowledged`)
    assert.equal(b.map.has('private'), false)
    if (v === 4) {
      const x = await provider(v, base, 'sessions', 'ok', { websocketProvider: ws, sessionAwareness: true })
      const y = await provider(v, base, 'sessions', 'ok', { websocketProvider: ws, sessionAwareness: true })
      await waitFor(() => synced(x) && synced(y), 'hp4: session-aware keys')
      x.map.set('session', 'same-document')
      await waitFor(() => y.map.get('session') === 'same-document', 'hp4: two session keys share a document')
      x.close()
      y.map.set('after-close', 'alive')
      await waitFor(() => !y.p.hasUnsyncedChanges, 'hp4: per-key close leaves its peer syncing')
    }
    a.close()
    b.map.set('survives', true)
    await waitFor(() => !b.p.hasUnsyncedChanges, `hp${v}: other document survives close`)
  }
  const noAuth = await start(true)
  const noToken = await provider(2, noAuth, 'no-token', null)
  await waitFor(() => synced(noToken), 'hp2: no token without an auth hook')

  // All versions must renew awareness and remain connected past the 30s
  // awareness timeout. Keep the same socket and compare close counts.
  console.log('Waiting 45 seconds for provider presence renewals')
  await sleep(45000)
  for (const { v, a, b, closes } of live) {
    assert.equal(a.events.closes.length, closes)
    assert.equal(b.p.awareness.getStates().get(a.doc.clientID)?.user, 'alice')
    console.log(`PASS hp${v}: presence survives 45 seconds on the same connection`)
  }

  // Exercise yserve's actual routing, including a provider stripping the
  // trailing slash off its configured URL.
  for (const prefix of ['/', '/ws/']) {
    const port = await new Promise(resolve => {
      const n = createServer(); n.listen(0, '127.0.0.1', () => { const p = n.address().port; n.close(() => resolve(p)) })
    })
    const proc = spawn(yserve, ['-addr', `127.0.0.1:${port}`, '-path-prefix', prefix], { stdio: ['ignore', 'ignore', 'pipe'] })
    servers.push(proc)
    let log = ''; proc.stderr.on('data', data => { log += data })
    await waitFor(() => log.includes('listening on'), 'yserve starts')
    const c = await provider(4, `ws://127.0.0.1:${port}${prefix}`, 'mounted')
    await waitFor(() => synced(c) && !c.p.hasUnsyncedChanges, `yserve: provider at ${prefix}`)
  }
  console.log('All Hocuspocus server checks passed')
} finally {
  for (const c of clients.reverse()) c.close()
  for (const proc of servers) {
    if (proc.exitCode === null) {
      const stopped = new Promise(resolve => proc.once('exit', resolve))
      proc.kill('SIGTERM')
      await stopped
    }
  }
}

// Providers retain retry timers after destroy, as in the client matrix.
process.exit(0)
