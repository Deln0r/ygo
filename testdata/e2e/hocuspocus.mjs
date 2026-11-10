// Runs the ygo client in Hocuspocus mode (testdata/e2e/hpclient) against
// stock @hocuspocus/server 4.8.0, 3.4.4 and 2.15.3, next to the stock
// provider of the same version, and exits non-zero if any check fails.
//
//   node hocuspocus.mjs path/to/hpclient
//
// Each server runs an onAuthenticate hook that accepts "ok", refuses "bad"
// with the reason "bad-token" and makes "ro" read-only, and an onStateless
// hook that answers "echo:X" with "reply:X". Server 2 also runs without
// the hook, which takes another path in it.
import { spawn } from 'node:child_process'
import { createInterface } from 'node:readline'
import WebSocket from 'ws'
import * as Y from 'yjs'

const bin = process.argv[2]
if (!bin) {
  console.error('usage: node hocuspocus.mjs path/to/hpclient')
  process.exit(2)
}

const PACKAGES = {
  4: { server: '@hocuspocus/server', provider: '@hocuspocus/provider' },
  3: { server: 'hocuspocus-server-3', provider: 'hocuspocus-provider-3' },
  2: { server: 'hocuspocus-server-2', provider: 'hocuspocus-provider-2' }
}

const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms))

async function waitFor (cond, ms = 5000) {
  const start = Date.now()
  while (Date.now() - start < ms) {
    if (await cond()) return true
    await sleep(20)
  }
  return false
}

let failures = 0
function check (name, ok, detail = '') {
  console.log(`${ok ? 'ok  ' : 'FAIL'} ${name}${ok || !detail ? '' : `: ${detail}`}`)
  if (!ok) failures++
}

async function onAuthenticate (data) {
  if (data.token === 'bad') {
    const e = new Error('bad-token')
    e.reason = 'bad-token'
    throw e
  }
  if (data.token === 'ro') {
    if (data.connectionConfig) data.connectionConfig.readOnly = true
    if (data.connection) data.connection.readOnly = true
  }
  return {}
}

async function onStateless ({ payload, connection }) {
  if (payload.startsWith('echo:')) connection.sendStateless('reply:' + payload.slice(5))
}

async function startServer (v, { auth = true, timeout } = {}) {
  const mod = await import(PACKAGES[v].server)
  // Server 4 passes `address` to listen as the host. Servers 2 and 3 pass
  // it under a key node ignores, so they listen on every interface; on
  // macOS another local process bound to 127.0.0.1 on the same port then
  // takes the connections (seen once as 404s while other test servers
  // ran). CI runs this job alone.
  const opts = { port: 0, address: '127.0.0.1', quiet: true, stopOnSignals: false, onStateless }
  if (auth) opts.onAuthenticate = onAuthenticate
  if (timeout) opts.timeout = timeout
  const s = v === 2 ? new mod.Hocuspocus(opts) : new mod.Server(opts)
  await s.listen()
  return { url: `ws://127.0.0.1:${s.address.port}`, stop: () => s.destroy() }
}

async function provider (v, url, name, token = 'ok') {
  const { HocuspocusProvider } = await import(PACKAGES[v].provider)
  const doc = new Y.Doc()
  const p = new HocuspocusProvider({ url, name, document: doc, token, WebSocketPolyfill: WebSocket })
  return { p, doc, map: doc.getMap('m') }
}

function goClient (url, doc, { token = 'ok', awarenessTimeout = 0 } = {}) {
  const proc = spawn(bin, ['-url', url, '-doc', doc, '-token', token, '-awareness-timeout', String(awarenessTimeout)],
    { stdio: ['pipe', 'pipe', 'inherit'] })
  const events = []
  createInterface({ input: proc.stdout }).on('line', (l) => {
    try { events.push(JSON.parse(l)) } catch { events.push({ ev: 'garbage', line: l }) }
  })
  const exited = new Promise((resolve) => proc.on('exit', resolve))
  const send = (cmd) => proc.stdin.write(JSON.stringify(cmd) + '\n')
  async function event (pred, ms = 5000, from = 0) {
    let found = null
    await waitFor(() => {
      for (let i = from; i < events.length; i++) {
        if (pred(events[i])) { found = events[i]; return true }
      }
      return false
    }, ms)
    return found
  }
  async function ask (cmd, ev) {
    const from = events.length
    send(cmd)
    return event((e) => e.ev === ev, 5000, from)
  }
  async function close () {
    send({ op: 'close' })
    await Promise.race([exited, sleep(5000)])
    proc.kill()
  }
  return { events, send, event, ask, close }
}

const value = async (g, key) => (await g.ask({ op: 'get', key }, 'value'))?.value
const status = async (g) => g.ask({ op: 'status' }, 'status')

async function syncChecks (v, url, label, { auth = true } = {}) {
  const name = `sync-${label}`
  const js = await provider(v, url, name)
  await waitFor(() => js.p.isSynced || js.p.synced)
  js.map.set('k0', 'js-early')

  const g = goClient(url, name, { token: auth ? 'ok' : '' })
  const ready = await g.event((e) => e.ev === 'ready')
  if (auth) {
    const a = await g.event((e) => e.ev === 'authenticated')
    check(`${label}: authenticated read-write`, a?.scope === 'read-write', JSON.stringify(a))
  }
  check(`${label}: synced`, !!await g.event((e) => e.ev === 'synced' && e.v))
  check(`${label}: late joiner reads the provider's edit`, await waitFor(async () => await value(g, 'k0') === 'js-early'))

  g.send({ op: 'set', key: 'k1', value: 'go' })
  check(`${label}: a Go edit reaches the provider`, await waitFor(() => js.map.get('k1') === 'go'))
  js.map.set('k2', 'js')
  check(`${label}: a provider edit reaches Go`, await waitFor(async () => await value(g, 'k2') === 'js'))
  check(`${label}: Unsynced back to 0`, await waitFor(async () => (await status(g))?.unsynced === 0))

  g.send({ op: 'presence', state: '{"user":"go"}' })
  check(`${label}: the provider sees Go's presence`,
    await waitFor(() => js.p.awareness.getStates().get(ready.id)?.user === 'go'))
  js.p.setAwarenessField('user', 'js')
  check(`${label}: Go sees the provider's presence`,
    await waitFor(async () => ((await g.ask({ op: 'peer', id: js.doc.clientID }, 'peer'))?.state ?? '').includes('"js"')))

  g.send({ op: 'stateless', payload: 'echo:1' })
  check(`${label}: stateless both ways`, !!await g.event((e) => e.ev === 'stateless' && e.payload === 'reply:1'))

  await g.close()
  check(`${label}: no error over the session`, !g.events.some((e) => e.ev === 'error'),
    JSON.stringify(g.events.filter((e) => e.ev === 'error')))
  js.p.destroy()
}

async function authChecks (v, url, label) {
  const bad = goClient(url, `denied-${label}`, { token: 'bad' })
  const e = await bad.event((x) => x.ev === 'error')
  check(`${label}: a bad token is ErrPermissionDenied with the reason`, e?.kind === 'denied' && e.msg.includes('bad-token'), JSON.stringify(e))
  check(`${label}: a bad token never syncs`, !bad.events.some((x) => x.ev === 'synced' && x.v))
  await bad.close()

  const name = `ro-${label}`
  const js = await provider(v, url, name)
  await waitFor(() => js.p.isSynced || js.p.synced)
  const ro = goClient(url, name, { token: 'ro' })
  const a = await ro.event((x) => x.ev === 'authenticated')
  check(`${label}: read-only scope`, a?.scope === 'readonly', JSON.stringify(a))
  await ro.event((x) => x.ev === 'synced' && x.v)
  ro.send({ op: 'set', key: 'k3', value: 'ro' })
  const r = await ro.event((x) => x.ev === 'error')
  check(`${label}: a read-only edit is ErrUpdateRefused`, r?.kind === 'refused', JSON.stringify(r))
  await sleep(500)
  check(`${label}: the server did not apply it`, js.map.get('k3') === undefined)
  check(`${label}: Unsynced stays above 0`, (await status(ro))?.unsynced > 0)
  await ro.close()
  js.p.destroy()
}

async function timeoutChecks (url, label) {
  // Keepalive: no presence, no edits, a QueryAwareness every 500 ms.
  const keep = goClient(url, `keep-${label}`, { awarenessTimeout: 1000 })
  await keep.event((e) => e.ev === 'synced' && e.v)
  await sleep(4500)
  check(`${label}: keepalive holds past the server's timeout`, !keep.events.some((e) => e.ev === 'error'),
    JSON.stringify(keep.events.filter((e) => e.ev === 'error')))
  await keep.close()

  // Without it (keepalive every 30 s) the server closes the document.
  const idle = goClient(url, `idle-${label}`, { awarenessTimeout: 60000 })
  await idle.event((e) => e.ev === 'synced' && e.v)
  const from = idle.events.length
  const c = await idle.event((e) => e.ev === 'error', 6000)
  check(`${label}: the server's CLOSE is ErrDocumentClosed`, c?.kind === 'closed' && c.msg.includes('Connection Timeout'), JSON.stringify(c))
  check(`${label}: the client syncs again after it`, !!await idle.event((e) => e.ev === 'synced' && e.v, 5000, from + 1))
  await idle.close()
}

const runs = [
  { v: 4, label: 'server 4.8' },
  { v: 3, label: 'server 3.4' },
  { v: 2, label: 'server 2.15' }
]
for (const { v, label } of runs) {
  const s = await startServer(v)
  try {
    await syncChecks(v, s.url, label)
    await authChecks(v, s.url, label)
  } finally {
    await s.stop()
  }
}
{
  const s = await startServer(2, { auth: false })
  try {
    await syncChecks(2, s.url, 'server 2.15 without onAuthenticate', { auth: false })
  } finally {
    await s.stop()
  }
}
{
  const s = await startServer(4, { timeout: 1500 })
  try {
    await timeoutChecks(s.url, 'server 4.8, timeout 1.5 s')
  } finally {
    await s.stop()
  }
}

console.log(failures === 0 ? 'all checks passed' : `${failures} checks failed`)
process.exit(failures === 0 ? 0 : 1)
