const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')
const assert = require('node:assert/strict')
const ts = require('typescript')

const sourceRoot = path.join(__dirname, '..', 'src')
const component = fs.readFileSync(path.join(sourceRoot, 'components/ServersTab.vue'), 'utf8')
const script = component.match(/<script setup lang="ts">([\s\S]*?)<\/script>/)[1]
const exposed = ['groups', 'monitor', 'load', 'loadMonitor', 'healthOf', 'aliveText', 'monitorLine', 'canCheck', 'checkAll']
const code = ts.transpileModule(script + '\nexport const exposed = {' + exposed.join(',') + '}', {
  compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 },
}).outputText
const server = { name: 'Synthetic', fingerprint: 'abcdef01', address: '192.0.2.1', port: 443, ips: [], protocol: 'trojan' }
const group = { id: '0a1b2c3d', name: 'Synthetic', servers: [server] }
const row = { index: 0, fingerprint: server.fingerprint, status: 'alive', latency_ms: 142 }
const snapshot = { state: 'ok', interval_seconds: 60, lag_seconds: 0, subscriptions: [{ id: group.id, alive: 1, total: 1, servers: [row] }] }

function deferred() {
  let resolve, reject
  const promise = new Promise((yes, no) => { resolve = yes; reject = no })
  return { promise, resolve, reject }
}

function setup() {
  const pending = [], timers = new Map(), unmount = []
  let groups = [group]
  const api = {
    getMonitor: () => { const d = deferred(); pending.push(d); return d.promise },
    getSubscriptions: async () => ({ data: { subscriptions: [] } }),
    getServers: async () => ({ data: { subscriptions: groups, active: null } }),
    checkAllServers: async () => ({ data: { queued: 1 } }),
  }
  let timer = 0
  const vue = { ref: value => ({ value }), computed: fn => ({ get value() { return fn() } }), onMounted: () => {}, onUnmounted: fn => unmount.push(fn) }
  const context = { exports: {}, require: name => name === 'vue' ? vue : { default: api }, Date, AbortController,
    alert: () => {}, confirm: () => true,
    setTimeout: (fn, ms) => { timers.set(++timer, { fn, ms }); return timer }, clearTimeout: id => timers.delete(id),
    setInterval: (fn, ms) => { timers.set(++timer, { fn, ms }); return timer }, clearInterval: id => timers.delete(id) }
  vm.runInNewContext(code, context)
  const x = context.exports.exposed
  x.groups.value = [group]
  return { x, api, pending, timers, unmount, setGroups: value => { groups = value } }
}

let passes = 0, failures = 0
async function test(name, fn) {
  try { await fn(); passes++; console.log('PASS ' + name) }
  catch (e) { failures++; console.error('FAIL ' + name + ': ' + e.message) }
}

;(async () => {
  await test('D8 old success cannot overwrite newer unavailability', async () => {
    const c = setup()
    const old = c.x.loadMonitor(), fresh = c.x.loadMonitor()
    c.pending[1].reject(Error('offline')); await fresh
    c.pending[0].resolve({ data: snapshot }); await old
    assert.equal(c.x.monitorLine.value, 'Monitoring: unavailable')
    assert.equal(c.x.canCheck.value, false)
    assert.equal(c.x.aliveText(group.id), '')
    assert.equal(c.x.healthOf(group, 0, server), null)
  })
  await test('D8 old failure cannot hide newer recovery', async () => {
    const c = setup()
    const old = c.x.loadMonitor(), fresh = c.x.loadMonitor()
    c.pending[1].resolve({ data: snapshot }); await fresh
    c.pending[0].reject(Error('old offline')); await old
    assert.equal(c.x.monitorLine.value, 'Monitoring every 1 min')
    assert.equal(c.x.canCheck.value, true)
    assert.equal(c.x.aliveText(group.id), '1/1 alive')
  })
  await test('D8 reverse successes retain the latest request', async () => {
    const c = setup()
    const old = c.x.loadMonitor(), fresh = c.x.loadMonitor()
    c.pending[1].resolve({ data: { ...snapshot, state: 'wan_down' } }); await fresh
    c.pending[0].resolve({ data: snapshot }); await old
    assert.equal(c.x.monitorLine.value, 'Monitoring: WAN down, statuses kept')
  })
  await test('D8 the actual monitor client calls are bounded', () => {
    const calls = []
    const http = { get: (...args) => { calls.push(args); return Promise.resolve({}) }, post: (...args) => { calls.push(args); return Promise.resolve({}) }, interceptors: { response: { use: () => {} } } }
    const context = { exports: {}, require: () => ({ default: { create: () => http } }) }
    const code = ts.transpileModule(fs.readFileSync(path.join(sourceRoot, 'api.ts'), 'utf8'), { compilerOptions: { module: ts.ModuleKind.CommonJS } }).outputText
    vm.runInNewContext(code, context)
    const api = context.exports.default
    api.getMonitor(); api.checkServer(group.id, 0, server.fingerprint); api.checkAllServers()
    assert.equal(calls.length, 3)
    for (const call of calls) assert.equal(call.at(-1)?.timeout, 8000)
    assert.equal(JSON.stringify(calls[1][1]), JSON.stringify({ subscription: group.id, index: 0, fingerprint: server.fingerprint }))
    assert.equal(JSON.stringify(calls[2][1]), '{}')
  })
  for (const [name, changed] of [
    ['changed count', { ...group, servers: [server, { ...server, fingerprint: 'abcdef02' }] }],
    ['same-count replacement', { ...group, servers: [{ ...server, fingerprint: 'abcdef02' }] }],
  ]) {
    await test('D9 hide stale aggregates after ' + name, async () => {
      const c = setup()
      const first = c.x.loadMonitor(); c.pending[0].resolve({ data: snapshot }); await first
      assert.equal(c.x.aliveText(group.id), '1/1 alive')
      c.setGroups([changed]); await c.x.load()
      assert.equal(c.x.aliveText(group.id), '')
      assert.equal(c.x.healthOf(changed, changed.servers.length - 1, changed.servers.at(-1)), null)
      const updated = { ...snapshot, subscriptions: [{ id: group.id, alive: changed.servers.length, total: changed.servers.length, servers: changed.servers.map((s, index) => ({ ...row, index, fingerprint: s.fingerprint })) }] }
      const fresh = c.x.loadMonitor(); c.pending[1].resolve({ data: updated }); await fresh
      assert.equal(c.x.aliveText(group.id), `${changed.servers.length}/${changed.servers.length} alive`)
    })
  }
  await test('D9 malformed aggregate denominator/index stays hidden', async () => {
    const c = setup()
    const request = c.x.loadMonitor()
    c.pending[0].resolve({ data: { ...snapshot, subscriptions: [{ ...snapshot.subscriptions[0], total: 2 }] } }); await request
    assert.equal(c.x.aliveText(group.id), '')
    c.x.monitor.value = { ...snapshot, subscriptions: [{ ...snapshot.subscriptions[0], servers: [{ ...row, index: 1 }] }] }
    assert.equal(c.x.aliveText(group.id), '')
  })
  await test('unmounted requests and delayed timers stay inactive', async () => {
    const c = setup()
    const request = c.x.loadMonitor()
    c.unmount.forEach(fn => fn())
    c.pending[0].resolve({ data: snapshot }); await request
    assert.equal(c.x.monitor.value, null)
    await c.x.checkAll()
    assert.equal(c.timers.size, 0)
    const late = c.x.loadMonitor()
    if (c.pending[1]) c.pending[1].resolve({ data: snapshot })
    await late
    assert.equal(c.pending.length, 1)
  })
  console.log(`RESULT ${passes} PASS / ${failures} FAIL`)
  process.exitCode = failures ? 1 : 0
})()
