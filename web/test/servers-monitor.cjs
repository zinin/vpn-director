const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')
const assert = require('node:assert/strict')
const ts = require('typescript')

const sourceRoot = path.join(__dirname, '..', 'src')
const component = fs.readFileSync(path.join(sourceRoot, 'components/ServersTab.vue'), 'utf8')
// watchd's periodic refresh writes a list only when it changed: the column
// shows when the list last changed, not when it was last checked.
assert.match(component, /<th>Changed<\/th>/)
assert.doesNotMatch(component, /<th>Refreshed<\/th>/)
const script = component.match(/<script setup lang="ts">([\s\S]*?)<\/script>/)[1]
const exposed = ['groups', 'monitor', 'load', 'loadMonitor', 'healthOf', 'aliveText', 'monitorLine', 'canCheck', 'checkAll', 'checkServer', 'checking']
const code = ts.transpileModule(script + '\nexport const exposed = {' + exposed.join(',') + ', checkNotice: typeof checkNotice === "undefined" ? undefined : checkNotice}', {
  compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 },
}).outputText
const monitorModule = { exports: {} }
vm.runInNewContext(ts.transpileModule(fs.readFileSync(path.join(sourceRoot, 'monitor.ts'), 'utf8'), {
  compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 },
}).outputText, monitorModule)
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
  const pending = [], timers = new Map(), unmount = [], checks = [], alerts = []
  let groups = [group]
  let checkResult = async () => ({ data: { queued: 1 } })
  const api = {
    getMonitor: () => { const d = deferred(); pending.push(d); return d.promise },
    getSubscriptions: async () => ({ data: { subscriptions: [] } }),
    getServers: async () => ({ data: { subscriptions: groups, active: null } }),
    getWatch: async () => ({ data: { state: 'active', updated_at: '2026-10-07T12:00:00Z', committed_failover: false, pending_restore: false, notifications: { pending: 0 } } }),
    checkServer: (...args) => { checks.push({ kind: 'one', args }); return checkResult() },
    checkAllServers: () => { checks.push({ kind: 'all', args: [] }); return checkResult() },
  }
  let timer = 0
  const vue = { ref: value => ({ value }), computed: fn => ({ get value() { return fn() } }), onMounted: () => {}, onUnmounted: fn => unmount.push(fn) }
  const context = { exports: {}, require: name => name === 'vue' ? vue
    : name === '../watch' ? require('./watch-status.cjs').loadWatchModule()
    : name === '../monitor' ? monitorModule.exports
    : name === './WatchStatus.vue' ? { default: { filename: path.join(sourceRoot, 'components/WatchStatus.vue') } }
    : { default: api }, Date, AbortController,
    alert: message => alerts.push(message), confirm: () => true,
    setTimeout: (fn, ms) => { timers.set(++timer, { fn, ms }); return timer }, clearTimeout: id => timers.delete(id),
    setInterval: (fn, ms) => { timers.set(++timer, { fn, ms }); return timer }, clearInterval: id => timers.delete(id) }
  vm.runInNewContext(code, context)
  const x = context.exports.exposed
  x.groups.value = [group]
  return { x, api, pending, timers, unmount, checks, alerts, setGroups: value => { groups = value }, setCheck: fn => { checkResult = fn } }
}

let passes = 0, failures = 0
async function test(name, fn) {
  try { await fn(); passes++; console.log('PASS ' + name) }
  catch (e) { failures++; console.error('FAIL ' + name + ': ' + e.message) }
}

const queuedNotice = 'Check queued; waiting for WAN recovery.'
const wanSnapshot = { ...snapshot, state: 'wan_down' }
const notice = c => c.x.checkNotice?.value ?? ''
async function acceptMonitor(c, data) {
  const request = c.x.loadMonitor()
  c.pending.at(-1).resolve({ data })
  await request
}
async function showWANNotice(c) {
  await acceptMonitor(c, wanSnapshot)
  await c.x.checkAll()
  assert.equal(notice(c), queuedNotice)
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
  for (const kind of ['one', 'all']) {
    await test(`WAN notice follows successful ${kind} check with unchanged identity and timing`, async () => {
      const c = setup()
      await acceptMonitor(c, wanSnapshot)
      const post = deferred()
      c.setCheck(() => post.promise)
      const request = kind === 'one' ? c.x.checkServer(group, 0, server) : c.x.checkAll()
      assert.equal(notice(c), '')
      assert.equal(c.x.checking.value, kind === 'one' ? '0a1b2c3d:0' : 'all')
      assert.deepEqual(c.checks, [{ kind, args: kind === 'one' ? ['0a1b2c3d', 0, 'abcdef01'] : [] }])
      post.resolve({ data: { queued: 1 } }); await request
      assert.equal(notice(c), queuedNotice)
      assert.equal(c.x.checking.value, '')
      assert.equal(c.x.canCheck.value, true)
      assert.equal(c.x.monitorLine.value, 'Monitoring: WAN down, statuses kept')
      assert.equal(c.x.aliveText(group.id), '1/1 alive')
      assert.equal(c.x.healthOf(group, 0, server).status, 'alive')
      assert.equal(c.pending.length, 1)
      assert.equal(c.timers.size, 1)
      const timer = [...c.timers.values()][0]
      assert.equal(timer.ms, 3000)
      const poll = timer.fn()
      c.pending.at(-1).resolve({ data: wanSnapshot }); await poll
      assert.equal(notice(c), queuedNotice)
    })
    await test(`normal OK ${kind} check keeps the WAN notice empty`, async () => {
      const c = setup()
      await acceptMonitor(c, snapshot)
      await (kind === 'one' ? c.x.checkServer(group, 0, server) : c.x.checkAll())
      assert.equal(notice(c), '')
      assert.equal(c.x.monitorLine.value, 'Monitoring every 1 min')
      assert.equal(c.x.canCheck.value, true)
      assert.equal([...c.timers.values()][0].ms, 3000)
    })
  }
  await test('successful check uses the latest accepted WAN state', async () => {
    const c = setup()
    await acceptMonitor(c, snapshot)
    const post = deferred()
    c.setCheck(() => post.promise)
    const request = c.x.checkAll()
    await acceptMonitor(c, wanSnapshot)
    post.resolve({ data: { queued: 1 } }); await request
    assert.equal(notice(c), queuedNotice)
  })
  await test('latest WAN response retains the queued notice', async () => {
    const c = setup()
    await showWANNotice(c)
    await acceptMonitor(c, wanSnapshot)
    assert.equal(notice(c), queuedNotice)
    assert.equal(c.x.canCheck.value, true)
  })
  await test('latest recovery clears the queued notice', async () => {
    const c = setup()
    await showWANNotice(c)
    await acceptMonitor(c, snapshot)
    assert.equal(notice(c), '')
    assert.equal(c.x.monitorLine.value, 'Monitoring every 1 min')
  })
  await test('latest monitor failure clears the queued notice', async () => {
    const c = setup()
    await showWANNotice(c)
    const request = c.x.loadMonitor()
    c.pending.at(-1).reject(Error('offline')); await request
    assert.equal(notice(c), '')
    assert.equal(c.x.monitorLine.value, 'Monitoring: unavailable')
    assert.equal(c.x.canCheck.value, false)
  })
  for (const stale of ['recovery', 'failure']) {
    await test(`stale monitor ${stale} cannot clear the latest WAN notice`, async () => {
      const c = setup()
      await showWANNotice(c)
      const old = c.x.loadMonitor(), oldResponse = c.pending.at(-1)
      await acceptMonitor(c, wanSnapshot)
      if (stale === 'failure') oldResponse.reject(Error('old offline'))
      else oldResponse.resolve({ data: snapshot })
      await old
      assert.equal(notice(c), queuedNotice)
      assert.equal(c.x.monitorLine.value, 'Monitoring: WAN down, statuses kept')
    })
  }
  for (const latest of ['recovery', 'failure']) {
    await test(`late WAN response cannot restore notice after latest ${latest}`, async () => {
      const c = setup()
      await showWANNotice(c)
      const old = c.x.loadMonitor(), oldResponse = c.pending.at(-1)
      const fresh = c.x.loadMonitor()
      if (latest === 'failure') c.pending.at(-1).reject(Error('offline'))
      else c.pending.at(-1).resolve({ data: snapshot })
      await fresh
      assert.equal(notice(c), '')
      oldResponse.resolve({ data: wanSnapshot }); await old
      assert.equal(notice(c), '')
      assert.equal(c.x.monitorLine.value, latest === 'failure' ? 'Monitoring: unavailable' : 'Monitoring every 1 min')
    })
  }
  await test('another attempt clears notice before a failed POST and preserves its alert', async () => {
    const c = setup()
    await showWANNotice(c)
    const post = deferred()
    c.setCheck(() => post.promise)
    const request = c.x.checkServer(group, 0, server)
    assert.equal(notice(c), '')
    assert.equal(c.x.checking.value, '0a1b2c3d:0')
    post.reject({ response: { status: 503, data: { error: 'synthetic queue failure' } } }); await request
    assert.equal(notice(c), '')
    assert.equal(c.x.checking.value, '')
    assert.deepEqual(c.alerts, ['Error: synthetic queue failure'])
  })
  await test('changed-list POST clears notice and preserves 409 reload without an alert', async () => {
    const c = setup()
    await showWANNotice(c)
    const changed = { ...group, servers: [{ ...server, fingerprint: 'abcdef02' }] }
    c.setGroups([changed])
    c.setCheck(() => Promise.reject({ response: { status: 409, data: { error: 'server list changed' } } }))
    await c.x.checkAll()
    assert.equal(notice(c), '')
    assert.deepEqual(c.alerts, [])
    assert.equal(c.x.groups.value[0].servers[0].fingerprint, 'abcdef02')
    assert.equal(c.x.aliveText(group.id), '')
    assert.equal(c.x.checking.value, '')
  })
  await test('late successful POST cannot restore notice from an earlier check attempt', async () => {
    const c = setup()
    await acceptMonitor(c, wanSnapshot)
    const post = deferred()
    c.setCheck(() => post.promise)
    const old = c.x.checkServer(group, 0, server)
    c.setCheck(() => Promise.reject(Error('new check failed')))
    await c.x.checkAll()
    post.resolve({ data: { queued: 1 } }); await old
    assert.equal(notice(c), '')
    assert.deepEqual(c.alerts, ['Error: new check failed'])
    assert.equal([...c.timers.values()][0].ms, 3000)
  })
  for (const cleared of ['recovery', 'failure']) {
    await test(`late POST cannot restore notice after ${cleared} and a subsequent WAN response`, async () => {
      const c = setup()
      await acceptMonitor(c, wanSnapshot)
      const post = deferred()
      c.setCheck(() => post.promise)
      const request = c.x.checkAll()
      const fresh = c.x.loadMonitor()
      if (cleared === 'failure') c.pending.at(-1).reject(Error('offline'))
      else c.pending.at(-1).resolve({ data: snapshot })
      await fresh
      await acceptMonitor(c, wanSnapshot)
      post.resolve({ data: { queued: 1 } }); await request
      assert.equal(notice(c), '')
      assert.equal(c.x.monitorLine.value, 'Monitoring: WAN down, statuses kept')
    })
  }
  await test('unmount clears an existing notice and late monitor responses stay inactive', async () => {
    const c = setup()
    await showWANNotice(c)
    const request = c.x.loadMonitor()
    c.unmount.forEach(fn => fn())
    assert.equal(notice(c), '')
    c.pending.at(-1).resolve({ data: wanSnapshot }); await request
    assert.equal(notice(c), '')
    assert.equal(c.timers.size, 0)
  })
  await test('unmounted successful POST cannot restore notice or leak into a fresh instance', async () => {
    const old = setup()
    await acceptMonitor(old, wanSnapshot)
    const post = deferred()
    old.setCheck(() => post.promise)
    const request = old.x.checkAll()
    old.unmount.forEach(fn => fn())
    const fresh = setup()
    await acceptMonitor(fresh, wanSnapshot)
    post.resolve({ data: { queued: 1 } }); await request
    assert.equal(notice(old), '')
    assert.equal(old.timers.size, 0)
    assert.equal(notice(fresh), '')
    await fresh.x.checkAll()
    assert.equal(notice(fresh), queuedNotice)
    assert.equal(notice(old), '')
  })
  for (const kind of ['one', 'all']) {
    await test(`successful zero-queued WAN ${kind} check clears and leaves the notice empty`, async () => {
      const c = setup()
      await showWANNotice(c)
      c.setCheck(async () => ({ data: { queued: 0 } }))
      await (kind === 'one' ? c.x.checkServer(group, 0, server) : c.x.checkAll())
      assert.equal(notice(c), '')
      assert.equal(c.x.checking.value, '')
      assert.equal(c.x.canCheck.value, true)
      assert.equal(c.x.monitorLine.value, 'Monitoring: WAN down, statuses kept')
      assert.equal(c.x.aliveText(group.id), '1/1 alive')
      assert.equal(c.x.healthOf(group, 0, server).status, 'alive')
      assert.deepEqual(c.checks.at(-1), { kind, args: kind === 'one' ? ['0a1b2c3d', 0, 'abcdef01'] : [] })
      assert.equal([...c.timers.values()][0].ms, 3000)
    })
  }
  const watchVM = require('./watch-status.cjs')
  await test('Task14 watch outage and storage recovery preserve WAN notice and real health', async () => {
    const http = watchVM.makeHTTP()
    http.set('/api/monitor', watchVM.wanHealth)
    const c = watchVM.setupPage('Servers', http)
    await c.mount()
    const check = watchVM.click(c, 'Check all now')
    const post = http.pending('/api/monitor/check')[0]
    assert.equal(JSON.stringify(post.body), '{}')
    assert.equal(post.options.timeout, 8000)
    assert.equal(c.x.checking.value, 'all')
    assert.equal(c.x.checkNotice.value, '')
    http.answer(post, { queued: 1 }); await check
    assert.equal(c.x.checkNotice.value, queuedNotice)
    assert.equal(c.x.checking.value, '')
    http.fail('/api/watch')
    await c.x.load(); await watchVM.flush()
    assert.equal(watchVM.watchProps(c).unavailable, true)
    assert.equal(c.x.checkNotice.value, queuedNotice)
    assert.equal(c.x.monitorLine.value, 'Monitoring: WAN down, statuses kept')
    assert.equal(c.x.canCheck.value, true)
    assert.equal(c.x.aliveText('0a1b2c3d'), '2/2 alive')
    assert.equal(c.x.healthOf(watchVM.group, 0, watchVM.primary).status, 'alive')
    assert.equal(c.x.isActive('0a1b2c3d', watchVM.primary), true)
    http.recover('/api/watch')
    http.set('/api/watch', { ...watchVM.watch, notifications: { pending: 2, storage_error: 'Notification storage is unavailable' } })
    await c.x.load(); await watchVM.flush()
    assert.equal(watchVM.watchProps(c).unavailable, false)
    assert.equal(watchVM.watchProps(c).snapshot.state, 'active')
    assert.match(watchVM.watchDisplay(c), /Notification storage is unavailable/)
    assert.equal(c.x.checkNotice.value, queuedNotice)
    assert.equal(c.x.healthOf(watchVM.group, 0, watchVM.primary).latency_ms, 142)
    assert.equal(http.calls('/api/monitor/check', 'POST').length, 1)
    const recheck = [...c.timers.entries()].find(([, t]) => t.kind === 'timeout')
    assert.equal(recheck[1].ms, 3000)
    await c.fire(recheck[0])
    assert.equal(c.x.checkNotice.value, queuedNotice)
    assert.equal(c.x.canCheck.value, true)
    c.unmount()
    assert.equal(c.timers.size, 0)
  })
  await test('Task14 monitor failure alone clears WAN notice without hiding active watch', async () => {
    const http = watchVM.makeHTTP()
    http.set('/api/monitor', watchVM.wanHealth)
    const c = watchVM.setupPage('Servers', http)
    await c.mount()
    const check = watchVM.click(c, 'Check all now')
    http.answer(http.pending('/api/monitor/check')[0], { queued: 1 }); await check
    assert.equal(c.x.checkNotice.value, queuedNotice)
    http.fail('/api/monitor')
    await c.x.loadMonitor(); await watchVM.flush()
    assert.equal(c.x.checkNotice.value, '')
    assert.equal(c.x.monitorLine.value, 'Monitoring: unavailable')
    assert.equal(c.x.canCheck.value, false)
    assert.equal(c.x.aliveText('0a1b2c3d'), '')
    assert.equal(c.x.healthOf(watchVM.group, 0, watchVM.primary), null)
    assert.equal(watchVM.watchProps(c).snapshot.state, 'active')
    assert.equal(watchVM.watchProps(c).unavailable, false)
    assert.equal(c.x.isActive('0a1b2c3d', watchVM.primary), true)
    http.recover('/api/monitor')
    http.set('/api/watch', { ...watchVM.watch, state: 'incompatible', message: 'Bot compatibility is unconfirmed' })
    await c.x.load(); await c.x.loadMonitor(); await watchVM.flush()
    assert.equal(c.x.checkNotice.value, '')
    assert.equal(c.x.canCheck.value, true)
    assert.equal(c.x.aliveText('0a1b2c3d'), '2/2 alive')
    assert.equal(watchVM.watchProps(c).snapshot.state, 'incompatible')
    assert.match(watchVM.watchDisplay(c), /Bot compatibility is unconfirmed/)
    assert.equal(http.calls('/api/monitor/check', 'POST').length, 1)
    c.unmount()
  })
  await test('Task14 the shared poll keeps watch and monitor request generations separate', async () => {
    const http = watchVM.makeHTTP()
    http.set('/api/monitor', watchVM.wanHealth)
    const c = watchVM.setupPage('Servers', http)
    await c.mount()
    const check = watchVM.click(c, 'Check all now')
    http.answer(http.pending('/api/monitor/check')[0], { queued: 1 }); await check
    assert.equal(c.x.checkNotice.value, queuedNotice)
    http.hold('/api/monitor'); http.hold('/api/watch')
    const polls = [...c.timers.values()].filter(t => t.kind === 'interval')
    assert.equal(polls.length, 1)
    polls[0].fn(); await watchVM.flush()
    polls[0].fn(); await watchVM.flush()
    assert.equal(http.pending('/api/watch').length, 2)
    assert.equal(http.pending('/api/monitor').length, 2)
    const [oldWatch, newWatch] = http.pending('/api/watch')
    const [oldMonitor, newMonitor] = http.pending('/api/monitor')
    http.reject(newWatch); http.answer(newMonitor, watchVM.wanHealth)
    await watchVM.flush()
    http.answer(oldWatch, watchVM.watch); http.reject(oldMonitor)
    await watchVM.flush()
    assert.equal(watchVM.watchProps(c).unavailable, true)
    assert.equal(c.x.checkNotice.value, queuedNotice)
    assert.equal(c.x.monitorLine.value, 'Monitoring: WAN down, statuses kept')
    assert.equal(c.x.canCheck.value, true)
    assert.equal(c.x.aliveText('0a1b2c3d'), '2/2 alive')
    assert.equal(c.x.healthOf(watchVM.group, 0, watchVM.primary).fingerprint, 'abcdef01')
    assert.equal(http.calls('/api/monitor/check', 'POST').length, 1)
    const callbacks = [...c.timers.values()].map(t => t.fn)
    c.unmount()
    const requests = http.requests.length
    callbacks.forEach(fn => fn()); await watchVM.flush()
    assert.equal(c.x.checkNotice.value, '')
    assert.equal(c.timers.size, 0)
    assert.equal(http.requests.length, requests)
  })
  await test('Task14 selection and check 409 reloads remain usable during watch outage', async () => {
    const http = watchVM.makeHTTP()
    http.fail('/api/watch')
    const c = watchVM.setupPage('Servers', http)
    await c.mount()
    const select = watchVM.click(c, 'Select')
    const selectPost = http.pending('/api/servers/active')[0]
    assert.equal(JSON.stringify(selectPost.body), JSON.stringify({ subscription: '0a1b2c3d', index: 0, name: 'Primary', address: '192.0.2.1', port: 443 }))
    http.set('/api/servers', { subscriptions: [{ ...watchVM.group, name: 'Reloaded synthetic' }], active: { subscription: '0a1b2c3d', name: 'Primary', address: '192.0.2.1', port: 443 } })
    http.reject(selectPost, { response: { status: 409, data: { error: 'server list changed' } } }); await select
    await watchVM.flush()
    assert.deepEqual(c.alerts, ['The server list changed; it has been reloaded. Select the server again.'])
    assert.equal(c.x.groups.value[0].name, 'Reloaded synthetic')
    assert.equal(c.x.busy.value, '')
    assert.equal(watchVM.watchProps(c).unavailable, true)
    assert.equal(c.x.canCheck.value, true)
    const check = c.x.checkServer(c.x.groups.value[0], 0, c.x.groups.value[0].servers[0])
    const checkPost = http.pending('/api/monitor/check')[0]
    assert.equal(JSON.stringify(checkPost.body), '{"subscription":"0a1b2c3d","index":0,"fingerprint":"abcdef01"}')
    http.set('/api/servers', { subscriptions: [{ ...watchVM.group, name: 'Checked reload' }], active: null })
    http.reject(checkPost, { response: { status: 409, data: { error: 'server list changed' } } }); await check
    await watchVM.flush()
    assert.deepEqual(c.alerts, ['The server list changed; it has been reloaded. Select the server again.'])
    assert.equal(c.x.groups.value[0].name, 'Checked reload')
    assert.equal(c.x.checking.value, '')
    assert.equal(c.x.checkNotice.value, '')
    assert.equal(watchVM.watchProps(c).unavailable, true)
    assert.equal(http.calls('/api/servers/active', 'POST').length, 1)
    assert.equal(http.calls('/api/monitor/check', 'POST').length, 1)
    c.unmount()
    assert.equal(c.timers.size, 0)
  })
  await test('Task14 I1 mounted selection 500 reconciles committed active B with unchanged fingerprints', async () => {
    const http = watchVM.makeHTTP()
    http.set('/api/servers', { subscriptions: [watchVM.group], active: { subscription: '0a1b2c3d', name: 'Primary', address: '192.0.2.1', port: 443, seq: 1 } })
    const c = watchVM.setupPage('Servers', http)
    await c.mount()
    assert.equal(c.x.isActive('0a1b2c3d', watchVM.primary), true)
    assert.equal(c.x.isActive('0a1b2c3d', watchVM.group.servers[1]), false)
    assert.equal(c.x.active.value.seq, 1)
    assert.match(watchVM.text(c.render()), /Primary Active/)
    const start = http.requests.length
    const action = watchVM.click(c, 'Select', 1)
    const post = http.pending('/api/servers/active')[0]
    assert.equal(JSON.stringify(post.body), '{"subscription":"0a1b2c3d","index":1,"name":"Secondary","address":"198.51.100.2","port":8443}')
    assert.equal(c.x.busy.value, 'select:0a1b2c3d:1')
    const duplicate = c.x.selectServer(c.x.groups.value[0], 1)
    assert.equal(http.calls('/api/servers/active', 'POST').length, 1)
    await duplicate
    assert.equal(http.requests.slice(start).filter(r => r.method === 'GET').length, 0)
    http.set('/api/servers', { subscriptions: [watchVM.group], active: { subscription: '0a1b2c3d', name: 'Secondary', address: '198.51.100.2', port: 8443, seq: 2 } })
    http.set('/api/watch', { ...watchVM.watch, updated_at: '2026-10-07T12:00:01Z', notifications: { pending: 2 } })
    http.set('/api/monitor', { ...watchVM.health, subscriptions: [{ id: '0a1b2c3d', alive: 2, total: 2, servers: watchVM.health.subscriptions[0].servers.map((h, index) => ({ ...h, latency_ms: index === 1 ? 82 : 142 })) }] })
    http.reject(post, { response: { status: 500, data: { error: 'failed to restart xray: restart xray failed (exit 7): Synthetic browser control failure' } } })
    await action
    await watchVM.flush()
    assert.deepEqual(c.alerts, ['Error: failed to restart xray: restart xray failed (exit 7): Synthetic browser control failure'])
    for (const url of ['/api/subscriptions', '/api/servers', '/api/watch', '/api/monitor']) {
      assert.equal(http.requests.slice(start).filter(r => r.method === 'GET' && r.url === url).length, 1, 'Mounted selection 500 must reread ' + url)
    }
    assert.equal(http.requests.slice(start).filter(r => r.method !== 'GET').length, 1)
    assert.equal(c.x.active.value.subscription, '0a1b2c3d')
    assert.equal(c.x.active.value.name, 'Secondary')
    assert.equal(c.x.active.value.address, '198.51.100.2')
    assert.equal(c.x.active.value.port, 8443)
    assert.equal(c.x.active.value.seq, 2)
    assert.equal(c.x.groups.value.length, 1)
    assert.equal(c.x.groups.value[0].servers.length, 2)
    assert.equal(c.x.groups.value[0].servers.map(s => s.fingerprint).join(','), 'abcdef01,abcdef02')
    assert.equal(c.x.groups.value[0].servers.map(s => s.protocol).join(','), 'trojan,trojan')
    assert.equal(c.x.isActive('0a1b2c3d', watchVM.primary), false)
    assert.equal(c.x.isActive('0a1b2c3d', watchVM.group.servers[1]), true)
    assert.match(watchVM.text(c.render()), /Secondary Active/)
    assert.doesNotMatch(watchVM.text(c.render()), /Primary Active/)
    assert.equal(watchVM.watchProps(c).snapshot.updated_at, '2026-10-07T12:00:01Z')
    assert.equal(watchVM.watchProps(c).snapshot.state, 'active')
    assert.equal(watchVM.watchProps(c).snapshot.notifications.pending, 2)
    assert.equal(watchVM.watchProps(c).unavailable, false)
    assert.equal(c.x.healthOf(c.x.groups.value[0], 1, c.x.groups.value[0].servers[1]).latency_ms, 82)
    assert.equal(c.x.aliveText('0a1b2c3d'), '2/2 alive')
    assert.equal(c.x.checkNotice.value, '')
    assert.equal(c.x.busy.value, '')
    assert.equal(c.x.loading.value, false)
    assert.equal(c.x.error.value, '')
    assert.equal(c.timers.size, 1)
    assert.equal([...c.timers.values()][0].kind, 'interval')
    assert.equal([...c.timers.values()][0].ms, 15000)
    c.unmount()
    assert.equal(c.timers.size, 0)
    const status = watchVM.setupPage('Status', http)
    await status.mount()
    assert.equal(status.x.activeLabel.value, 'Synthetic / Secondary')
    assert.equal(status.x.activeServer.value.seq, 2)
    assert.equal(watchVM.watchProps(status).snapshot.updated_at, '2026-10-07T12:00:01Z')
    assert.equal(http.calls('/api/servers/active', 'POST').length, 1)
    status.unmount()
  })
  await test('Task14 I1 selection 500 reread preserves queued WAN notice and 15s/3s timers', async () => {
    const http = watchVM.makeHTTP()
    http.set('/api/servers', { subscriptions: [watchVM.group], active: { subscription: '0a1b2c3d', name: 'Primary', address: '192.0.2.1', port: 443, seq: 1 } })
    http.set('/api/monitor', watchVM.wanHealth)
    const c = watchVM.setupPage('Servers', http)
    await c.mount()
    const check = watchVM.click(c, 'Check all now')
    http.answer(http.pending('/api/monitor/check')[0], { queued: 1 }); await check
    assert.equal(c.x.checkNotice.value, 'Check queued; waiting for WAN recovery.')
    const start = http.requests.length
    const action = watchVM.click(c, 'Select', 1)
    http.set('/api/servers', { subscriptions: [watchVM.group], active: { subscription: '0a1b2c3d', name: 'Secondary', address: '198.51.100.2', port: 8443, seq: 2 } })
    http.reject(http.pending('/api/servers/active')[0], { response: { status: 500, data: { error: 'failed to restart xray: restart xray failed (exit 7): Synthetic browser control failure' } } })
    await action
    await watchVM.flush()
    for (const url of ['/api/subscriptions', '/api/servers', '/api/watch', '/api/monitor']) {
      assert.equal(http.requests.slice(start).filter(r => r.method === 'GET' && r.url === url).length, 1)
    }
    assert.equal(c.x.isActive('0a1b2c3d', watchVM.group.servers[1]), true)
    assert.equal(c.x.checkNotice.value, 'Check queued; waiting for WAN recovery.')
    assert.equal(c.x.monitorLine.value, 'Monitoring: WAN down, statuses kept')
    assert.equal(c.x.canCheck.value, true)
    assert.equal(c.x.healthOf(c.x.groups.value[0], 1, c.x.groups.value[0].servers[1]).fingerprint, 'abcdef02')
    assert.equal(c.x.healthOf(c.x.groups.value[0], 1, c.x.groups.value[0].servers[1]).latency_ms, 81)
    assert.equal(watchVM.watchProps(c).snapshot.state, 'active')
    assert.deepEqual(c.alerts, ['Error: failed to restart xray: restart xray failed (exit 7): Synthetic browser control failure'])
    assert.equal(http.calls('/api/monitor/check', 'POST').length, 1)
    assert.equal(http.calls('/api/servers/active', 'POST').length, 1)
    assert.equal(http.requests.slice(start).filter(r => r.method !== 'GET').length, 1)
    assert.equal(c.timers.size, 2)
    assert.equal([...c.timers.values()].find(t => t.kind === 'interval').ms, 15000)
    const recheck = [...c.timers.entries()].find(([, t]) => t.kind === 'timeout')
    assert.equal(recheck[1].ms, 3000)
    await c.fire(recheck[0])
    assert.equal(c.x.checkNotice.value, 'Check queued; waiting for WAN recovery.')
    assert.equal(http.calls('/api/servers/active', 'POST').length, 1)
    c.unmount()
    assert.equal(c.timers.size, 0)
  })
  for (const departure of ['unmount', 'logout']) {
    await test(`Task14 I1 late selection 500 after ${departure} starts no reads alerts timers or auth`, async () => {
      const http = watchVM.makeHTTP()
      http.set('/api/servers', { subscriptions: [watchVM.group], active: { subscription: '0a1b2c3d', name: 'Primary', address: '192.0.2.1', port: 443, seq: 1 } })
      const c = watchVM.setupPage('Servers', http)
      await c.mount()
      const callbacks = [...c.timers.values()].map(t => t.fn)
      const action = watchVM.click(c, 'Select', 1)
      const post = http.pending('/api/servers/active')[0]
      assert.equal(JSON.stringify(post.body), '{"subscription":"0a1b2c3d","index":1,"name":"Secondary","address":"198.51.100.2","port":8443}')
      http.set('/api/servers', { subscriptions: [watchVM.group], active: { subscription: '0a1b2c3d', name: 'Secondary', address: '198.51.100.2', port: 8443, seq: 2 } })
      if (departure === 'logout') {
        const logout = c.x.api.logout()
        assert.equal(http.pending('/api/logout').length, 1)
        http.answer(http.pending('/api/logout')[0], { ok: true }); await logout
      }
      c.unmount()
      const start = http.requests.length
      http.reject(post, { response: { status: 500, data: { error: 'failed to restart xray: restart xray failed (exit 7): Synthetic browser control failure' } } })
      await action
      callbacks.forEach(fn => fn())
      await c.x.load(); await c.x.loadMonitor(); await c.x.pollStatus()
      const selection = c.x.selectServer(c.x.groups.value[0], 1)
      assert.equal(http.calls('/api/servers/active', 'POST').length, 1)
      await selection
      await watchVM.flush()
      assert.equal(http.requests.length, start)
      assert.deepEqual(c.alerts, [])
      assert.equal(c.timers.size, 0)
      assert.equal(c.x.active.value.name, 'Primary')
      assert.equal(c.x.active.value.seq, 1)
      assert.equal(c.x.busy.value, 'select:0a1b2c3d:1')
      assert.equal(c.x.checkNotice.value, '')
      assert.equal(watchVM.watchProps(c).snapshot.updated_at, '2026-10-07T12:00:00Z')
      assert.equal(http.calls('/api/servers/active', 'POST').length, 1)
      assert.equal(http.calls('/api/logout', 'POST').length, departure === 'logout' ? 1 : 0)
      assert.equal(http.calls('/api/version').length, 0)
      assert.equal(http.calls('/api/login', 'POST').length, 0)
    })
    await test(`Task14 I1 selection 500 reread cannot publish or continue after ${departure}`, async () => {
      const http = watchVM.makeHTTP()
      http.set('/api/servers', { subscriptions: [watchVM.group], active: { subscription: '0a1b2c3d', name: 'Primary', address: '192.0.2.1', port: 443, seq: 1 } })
      const c = watchVM.setupPage('Servers', http)
      await c.mount()
      for (const url of ['/api/subscriptions', '/api/servers', '/api/watch', '/api/monitor']) http.hold(url)
      const action = watchVM.click(c, 'Select', 1)
      try {
        http.set('/api/servers', { subscriptions: [watchVM.group], active: { subscription: '0a1b2c3d', name: 'Secondary', address: '198.51.100.2', port: 8443, seq: 2 } })
        http.reject(http.pending('/api/servers/active')[0], { response: { status: 500, data: { error: 'failed to restart xray: restart xray failed (exit 7): Synthetic browser control failure' } } })
        await watchVM.flush()
        for (const url of ['/api/subscriptions', '/api/servers', '/api/watch']) assert.equal(http.pending(url).length, 1, 'Mounted failed selection must start its guarded reread')
        assert.deepEqual(c.alerts, ['Error: failed to restart xray: restart xray failed (exit 7): Synthetic browser control failure'])
        if (departure === 'logout') {
          const logout = c.x.api.logout()
          http.answer(http.pending('/api/logout')[0], { ok: true }); await logout
        }
        const callbacks = [...c.timers.values()].map(t => t.fn)
        c.unmount()
        const start = http.requests.length
        for (const r of http.requests.filter(r => r.method === 'GET' && !r.settled)) {
          http.answer(r, {
            '/api/subscriptions': { subscriptions: [ { id: '0a1b2c3d', name: 'Synthetic', host: 'subscription.example.test', static: false, servers: 2, added: '2026-10-07T12:00:00Z', refreshed: '2026-10-07T12:00:00Z' } ] },
            '/api/servers': { subscriptions: [watchVM.group], active: { subscription: '0a1b2c3d', name: 'Secondary', address: '198.51.100.2', port: 8443, seq: 2 } },
            '/api/watch': { ...watchVM.watch, updated_at: '2026-10-07T12:00:01Z', pending_restore: true },
            '/api/monitor': { ...watchVM.health, state: 'stopped' },
          }[r.url])
        }
        await action
        callbacks.forEach(fn => fn())
        await c.x.pollStatus()
        await watchVM.flush()
        assert.equal(http.requests.length, start, 'Unmounted error reread must not dispatch its next read')
        assert.equal(c.x.active.value.name, 'Primary')
        assert.equal(c.x.active.value.seq, 1)
        assert.equal(c.x.groups.value[0].servers.map(s => s.fingerprint).join(','), 'abcdef01,abcdef02')
        assert.equal(watchVM.watchProps(c).snapshot.updated_at, '2026-10-07T12:00:00Z')
        assert.equal(watchVM.watchProps(c).snapshot.pending_restore, false)
        assert.equal(c.x.monitorLine.value, 'Monitoring every 1 min')
        assert.equal(c.x.busy.value, 'select:0a1b2c3d:1')
        assert.deepEqual(c.alerts, ['Error: failed to restart xray: restart xray failed (exit 7): Synthetic browser control failure'])
        assert.equal(c.timers.size, 0)
        assert.equal(http.calls('/api/servers/active', 'POST').length, 1)
        assert.equal(http.calls('/api/logout', 'POST').length, departure === 'logout' ? 1 : 0)
        assert.equal(http.calls('/api/version').length, 0)
        assert.equal(http.calls('/api/login', 'POST').length, 0)
      } finally {
        c.unmount()
        for (const r of http.requests.filter(r => !r.settled)) http.reject(r, Error('Synthetic test cleanup'))
        await action
        await watchVM.flush()
      }
    })
  }
  await test('Task14 I1 selection 409 retains changed-list alert and does not advance the active seq', async () => {
    const http = watchVM.makeHTTP()
    http.set('/api/servers', { subscriptions: [watchVM.group], active: { subscription: '0a1b2c3d', name: 'Primary', address: '192.0.2.1', port: 443, seq: 1 } })
    const c = watchVM.setupPage('Servers', http)
    await c.mount()
    const start = http.requests.length
    const action = watchVM.click(c, 'Select', 1)
    const post = http.pending('/api/servers/active')[0]
    assert.equal(JSON.stringify(post.body), '{"subscription":"0a1b2c3d","index":1,"name":"Secondary","address":"198.51.100.2","port":8443}')
    http.set('/api/watch', { ...watchVM.watch, state: 'incompatible', message: 'Bot compatibility is unconfirmed' })
    http.reject(post, { response: { status: 409, data: { error: 'server list changed' } } })
    await action
    await watchVM.flush()
    assert.deepEqual(c.alerts, ['The server list changed; it has been reloaded. Select the server again.'])
    for (const url of ['/api/subscriptions', '/api/servers', '/api/watch', '/api/monitor']) {
      assert.equal(http.requests.slice(start).filter(r => r.method === 'GET' && r.url === url).length, 1)
    }
    assert.equal(http.requests.slice(start).filter(r => r.method !== 'GET').length, 1)
    assert.equal(c.x.isActive('0a1b2c3d', watchVM.primary), true)
    assert.equal(c.x.isActive('0a1b2c3d', watchVM.group.servers[1]), false)
    assert.equal(c.x.active.value.seq, 1)
    assert.equal(c.x.groups.value[0].servers.map(s => s.fingerprint).join(','), 'abcdef01,abcdef02')
    assert.equal(watchVM.watchProps(c).snapshot.state, 'incompatible')
    assert.equal(c.x.healthOf(c.x.groups.value[0], 1, c.x.groups.value[0].servers[1]).latency_ms, 81)
    assert.equal(c.x.checkNotice.value, '')
    assert.equal(c.x.busy.value, '')
    assert.equal(c.x.error.value, '')
    assert.equal(c.timers.size, 1)
    assert.equal([...c.timers.values()][0].ms, 15000)
    c.unmount()
    assert.equal(c.timers.size, 0)
  })
  const passiveA = { subscription: watchVM.group.id, name: 'Primary', address: '192.0.2.1', port: 443, seq: 1 }
  const passiveB = { subscription: watchVM.group.id, name: 'Secondary', address: '198.51.100.2', port: 8443, seq: 2 }
  const passiveServers = active => ({ subscriptions: [watchVM.group], active })
  const passiveFailure = { response: { status: 500, data: { error: 'Synthetic passive read failure' } } }

  async function withPassivePage(fn, prepare = () => {}) {
    const http = watchVM.makeHTTP()
    http.set('/api/servers', passiveServers(passiveA))
    prepare(http)
    const c = watchVM.setupPage('Servers', http)
    try {
      await c.mount()
      await fn(c, http)
    } finally {
      c.unmount()
      for (const r of http.requests.filter(r => !r.settled)) http.reject(r, Error('Synthetic test cleanup'))
      await watchVM.flush()
    }
  }

  function passiveTimer(c) {
    const timers = [...c.timers.entries()].filter(([, t]) => t.kind === 'interval')
    assert.equal(timers.length, 1, 'Passive active reads must share the existing poll')
    assert.equal(timers[0][1].ms, 15000)
    return timers[0]
  }

  function visibleRows(c) {
    return { groups: c.x.groups.value, subscriptions: c.x.subscriptions.value,
      servers: c.x.groups.value.map(g => g.servers), rows: c.x.groups.value.flatMap(g => g.servers ?? []) }
  }

  function assertSameRows(c, before) {
    assert.equal(c.x.groups.value, before.groups, 'Passive reads must retain visible group identity')
    assert.equal(c.x.subscriptions.value, before.subscriptions, 'Passive reads must not reload subscription rows')
    const current = visibleRows(c)
    current.servers.forEach((servers, i) => assert.equal(servers, before.servers[i]))
    current.rows.forEach((server, i) => assert.equal(server, before.rows[i]))
    assert.equal(current.rows.map(s => s.fingerprint).join(','), before.rows.map(s => s.fingerprint).join(','))
  }

  function renderedNodes(tree) {
    if (Array.isArray(tree)) return tree.flatMap(renderedNodes)
    if (!tree || typeof tree !== 'object') return []
    return [tree, ...renderedNodes(tree.children)]
  }

  function assertPassiveBadges(c, selected) {
    assert.equal(JSON.stringify(c.x.active.value), JSON.stringify(selected))
    for (const g of c.x.groups.value) {
      assert.equal(c.x.runsFrom(g.id), g.id === selected?.subscription, 'Running badge must follow the active subscription')
      for (const s of g.servers ?? []) {
        const matches = selected != null && selected.subscription === g.id && selected.name === s.name && selected.address === s.address && selected.port === s.port
        assert.equal(c.x.isActive(g.id, s), matches, 'Active badge must match the authoritative row identity')
      }
      const details = renderedNodes(c.render()).find(n => n.type === 'details' && n.props.key === g.id)
      assert.ok(details, 'Expected the real subscription details')
      const running = renderedNodes(details).filter(n => n.type === 'span' && watchVM.text(n) === 'running')
      assert.equal(running.length, g.id === selected?.subscription ? 1 : 0)
    }
    const badges = renderedNodes(c.render()).filter(n => n.type === 'span' && watchVM.text(n) === 'Active')
    assert.equal(badges.length, selected ? 1 : 0)
    if (selected) assert.match(watchVM.text(c.render()), new RegExp(selected.name + ' Active'))
  }

  function assertPassiveReads(http, start) {
    const requests = http.requests.slice(start)
    for (const url of ['/api/servers', '/api/monitor', '/api/watch']) {
      assert.equal(requests.filter(r => r.method === 'GET' && r.url === url).length, 1, 'Passive poll must independently read ' + url)
    }
    assert.equal(requests.length, 3, 'An unchanged list needs only three independent GETs')
    assert.equal(requests.filter(r => r.method !== 'GET').length, 0, 'Polling must never mutate configuration')
  }

  await test('Task16 I16-F1 passive unchanged-list A to B and preferred B to A refresh Active without reloading rows', async () => {
    await withPassivePage(async (c, http) => {
      const before = visibleRows(c)
      assertPassiveBadges(c, passiveA)
      for (const active of [passiveB, { ...passiveA, seq: 3 }]) {
        http.set('/api/servers', passiveServers(active))
        const start = http.requests.length
        await c.fire(passiveTimer(c)[0])
        assertPassiveBadges(c, active)
        assertSameRows(c, before)
        assertPassiveReads(http, start)
        assert.equal(c.x.aliveText(watchVM.group.id), '2/2 alive')
        assert.equal(c.x.healthOf(c.x.groups.value[0], 1, c.x.groups.value[0].servers[1]).fingerprint, 'abcdef02')
        assert.equal(c.x.loading.value, false)
        assert.equal(c.x.error.value, '')
        assert.deepEqual(c.alerts, [])
      }
      assert.equal(http.calls('/api/servers').length, 3)
      assert.equal(http.calls('/api/subscriptions').length, 1)
      assert.equal(http.requests.filter(r => r.method !== 'GET').length, 0)
    })
  })

  await test('Task16 I16-F1 passive cross-subscription switch and return move running and Active with identical endpoints', async () => {
    const other = { id: '1b2c3d4e', name: 'Other synthetic', servers: [{ ...watchVM.primary, fingerprint: 'abcdef03' }] }
    const groups = [watchVM.group, other]
    const health = { ...watchVM.health, subscriptions: [...watchVM.health.subscriptions,
      { id: other.id, alive: 1, total: 1, servers: [{ ...watchVM.health.subscriptions[0].servers[0], fingerprint: 'abcdef03' }] }] }
    await withPassivePage(async (c, http) => {
      const before = visibleRows(c)
      assertPassiveBadges(c, passiveA)
      for (const active of [{ ...passiveA, subscription: other.id, seq: 2 }, { ...passiveA, seq: 3 }]) {
        http.set('/api/servers', { subscriptions: groups, active })
        const start = http.requests.length
        await c.fire(passiveTimer(c)[0])
        assertPassiveBadges(c, active)
        assertSameRows(c, before)
        assertPassiveReads(http, start)
        assert.equal(c.x.aliveText(other.id), '1/1 alive')
        assert.equal(c.x.healthOf(c.x.groups.value[1], 0, c.x.groups.value[1].servers[0]).fingerprint, 'abcdef03')
      }
      c.unmount()
      const status = watchVM.setupPage('Status', http)
      try {
        await status.mount()
        assert.equal(status.x.activeLabel.value, 'Synthetic / Primary')
        assert.equal(status.x.activeServer.value.seq, 3)
      } finally { status.unmount() }
      assert.equal(http.requests.filter(r => r.method !== 'GET').length, 0)
    }, http => {
      http.set('/api/servers', { subscriptions: groups, active: passiveA })
      http.set('/api/subscriptions', { subscriptions: groups.map(g => ({ id: g.id, name: g.name, host: 'subscription.example.test', static: false, servers: g.servers.length, added: '2026-10-07T12:00:00Z', refreshed: '2026-10-07T12:00:00Z' })) })
      http.set('/api/monitor', health)
    })
  })

  for (const state of ['disabled', 'not_running', 'GET500']) {
    await test(`Task16 I16-F1 passive active refresh is independent of ${state} monitor and watch`, async () => {
      await withPassivePage(async (c, http) => {
        const before = visibleRows(c)
        if (state === 'GET500') {
          http.fail('/api/monitor', passiveFailure)
          http.fail('/api/watch', passiveFailure)
        } else {
          http.set('/api/monitor', { ...watchVM.health, state })
          http.set('/api/watch', { ...watchVM.watch, state: state === 'disabled' ? 'active' : 'not_running' })
        }
        http.set('/api/servers', passiveServers(passiveB))
        const start = http.requests.length
        await c.fire(passiveTimer(c)[0])
        assertPassiveBadges(c, passiveB)
        assertSameRows(c, before)
        assertPassiveReads(http, start)
        assert.equal(c.x.canCheck.value, false)
        assert.equal(c.x.monitorLine.value, state === 'disabled' ? 'Monitoring: disabled in settings' : state === 'GET500' ? 'Monitoring: unavailable' : 'Monitoring: not running')
        assert.equal(watchVM.watchProps(c).unavailable, state === 'GET500')
        if (state !== 'GET500') assert.equal(watchVM.watchProps(c).snapshot.state, state === 'disabled' ? 'active' : 'not_running')
        assert.equal(http.calls('/api/subscriptions').length, 1)
        assert.deepEqual(c.alerts, [])
      })
    })
  }

  await test('Task16 I16-F1 pending monitor and watch cannot delay the independent passive active read', async () => {
    await withPassivePage(async (c, http) => {
      const before = visibleRows(c)
      http.hold('/api/monitor'); http.hold('/api/watch')
      http.set('/api/servers', passiveServers(passiveB))
      const start = http.requests.length
      const polling = passiveTimer(c)[1].fn()
      await watchVM.flush()
      assertPassiveReads(http, start)
      assert.equal(http.pending('/api/monitor').length, 1)
      assert.equal(http.pending('/api/watch').length, 1)
      assertPassiveBadges(c, passiveB)
      assertSameRows(c, before)
      assert.equal(c.x.loading.value, false)
      http.answer(http.pending('/api/monitor')[0], { ...watchVM.health, state: 'wan_down' })
      http.reject(http.pending('/api/watch')[0], passiveFailure)
      await polling
      assert.equal(c.x.monitorLine.value, 'Monitoring: WAN down, statuses kept')
      assert.equal(watchVM.watchProps(c).unavailable, true)
      assertPassiveBadges(c, passiveB)
    })
  })

  await test('Task16 I16-F1 passive GET500 and recovery preserve rows form rename busy error summaries and WAN notice without flicker', async () => {
    await withPassivePage(async (c, http) => {
      const check = watchVM.click(c, 'Check all now')
      http.answer(http.pending('/api/monitor/check')[0], { queued: 1 }); await check
      c.x.addUrl.value = 'https://subscription.example.test/new-list'
      c.x.addName.value = 'Unsaved synthetic'
      c.x.startRename(c.x.subscriptions.value[0])
      c.x.renameText.value = 'Unsaved rename'
      c.x.error.value = 'Synthetic foreground reload error'
      c.x.summaries.value = ['Synthetic: Imported 2 of 2 servers']
      const rename = c.x.saveRename(c.x.subscriptions.value[0])
      assert.equal(JSON.stringify(http.pending('/api/subscriptions/rename')[0].body), '{"name":"Unsaved rename"}')
      const before = visibleRows(c), summaries = c.x.summaries.value
      const assertIntent = () => {
        assertSameRows(c, before)
        assert.equal(c.x.addUrl.value, 'https://subscription.example.test/new-list')
        assert.equal(c.x.addName.value, 'Unsaved synthetic')
        assert.equal(c.x.renaming.value, watchVM.group.id)
        assert.equal(c.x.renameText.value, 'Unsaved rename')
        assert.equal(c.x.busy.value, 'rename:' + watchVM.group.id)
        assert.equal(c.x.error.value, 'Synthetic foreground reload error')
        assert.equal(c.x.summaries.value, summaries)
        assert.equal(c.x.checkNotice.value, queuedNotice)
        assert.equal(c.x.loading.value, false)
        assert.deepEqual(c.alerts, [])
      }
      http.hold('/api/servers')
      for (const outcome of ['GET500', 'recovery']) {
        http.set('/api/monitor', { ...watchVM.wanHealth, lag_seconds: outcome === 'GET500' ? 5 : 9 })
        http.set('/api/watch', { ...watchVM.watch, updated_at: outcome === 'GET500' ? '2026-10-07T12:00:01Z' : '2026-10-07T12:00:02Z', notifications: { pending: outcome === 'GET500' ? 1 : 2 } })
        const start = http.requests.length
        const polling = passiveTimer(c)[1].fn()
        await watchVM.flush()
        assert.equal(http.pending('/api/servers').length, 1, 'Passive polling must read authoritative active even during a control')
        assertPassiveReads(http, start)
        assertIntent()
        assert.equal(c.x.monitorLine.value, 'Monitoring: WAN down, statuses kept')
        assert.equal(c.x.monitor.value.lag_seconds, outcome === 'GET500' ? 5 : 9)
        assert.equal(watchVM.watchProps(c).snapshot.notifications.pending, outcome === 'GET500' ? 1 : 2)
        const read = http.pending('/api/servers')[0]
        if (outcome === 'GET500') http.reject(read, passiveFailure)
        else http.answer(read, passiveServers(passiveB))
        await polling
        assertIntent()
        assertPassiveBadges(c, outcome === 'GET500' ? passiveA : passiveB)
      }
      assert.equal(http.calls('/api/subscriptions').length, 1)
      assert.equal(http.calls('/api/monitor/check', 'POST').length, 1)
      assert.equal(http.calls('/api/subscriptions/rename', 'POST').length, 1)
      assert.equal(http.calls('/api/servers/active', 'POST').length, 0)
      assert.equal([...c.timers.values()].find(t => t.kind === 'timeout').ms, 3000)
      c.unmount()
      http.answer(http.pending('/api/subscriptions/rename')[0], { ok: true }); await rename
    }, http => http.set('/api/monitor', watchVM.wanHealth))
  })

  for (const outcome of ['success', 'GET500']) {
    await test(`Task16 I16-F1 stale passive ${outcome} cannot undo a newer preferred return`, async () => {
      await withPassivePage(async (c, http) => {
        const before = visibleRows(c)
        http.hold('/api/servers')
        const first = passiveTimer(c)[1].fn(); await watchVM.flush()
        const second = passiveTimer(c)[1].fn(); await watchVM.flush()
        assert.equal(http.pending('/api/servers').length, 2, 'Each overlapping poll needs an independent active read')
        const [old, fresh] = http.pending('/api/servers')
        const returned = { ...passiveA, seq: 3 }
        http.answer(fresh, passiveServers(returned)); await second
        assertPassiveBadges(c, returned)
        if (outcome === 'success') http.answer(old, passiveServers(passiveB))
        else http.reject(old, passiveFailure)
        await first
        assertPassiveBadges(c, returned)
        assertSameRows(c, before)
        assert.equal(c.x.error.value, '')
        assert.equal(c.x.loading.value, false)
        assert.deepEqual(c.alerts, [])
        assert.equal(http.calls('/api/servers').length, 3)
        assert.equal(http.calls('/api/monitor').length, 3)
        assert.equal(http.calls('/api/watch').length, 3)
        assert.equal(http.requests.filter(r => r.method !== 'GET').length, 0)
      })
    })
  }

  await test('Task16 I16-F1 latest passive GET500 fences older success and the next poll recovers', async () => {
    await withPassivePage(async (c, http) => {
      const before = visibleRows(c)
      http.hold('/api/servers')
      const first = passiveTimer(c)[1].fn(); await watchVM.flush()
      const second = passiveTimer(c)[1].fn(); await watchVM.flush()
      assert.equal(http.pending('/api/servers').length, 2)
      const [old, fresh] = http.pending('/api/servers')
      http.reject(fresh, passiveFailure); await second
      assertPassiveBadges(c, passiveA)
      http.answer(old, passiveServers(passiveB)); await first
      assertPassiveBadges(c, passiveA)
      http.recover('/api/servers')
      http.set('/api/servers', passiveServers({ ...passiveB, seq: 3 }))
      await c.fire(passiveTimer(c)[0])
      assertPassiveBadges(c, { ...passiveB, seq: 3 })
      assertSameRows(c, before)
      assert.equal(c.x.error.value, '')
      assert.deepEqual(c.alerts, [])
      assert.equal(http.calls('/api/servers').length, 4)
      assert.equal(http.calls('/api/subscriptions').length, 1)
      assert.equal(http.requests.filter(r => r.method !== 'GET').length, 0)
    })
  })

  for (const origin of ['initial', 'reload']) {
    await test(`Task16 I16-F1 late ${origin} list GET cannot restore active before a newer passive read`, async () => {
      await withPassivePage(async (c, http) => {
        let listRead
        if (origin === 'reload') {
          http.hold('/api/servers')
          listRead = c.x.load()
          await watchVM.flush()
        }
        assert.equal(http.pending('/api/servers').length, 1)
        const old = http.pending('/api/servers')[0]
        assert.equal(c.x.loading.value, true)
        const polling = passiveTimer(c)[1].fn(); await watchVM.flush()
        assert.equal(http.pending('/api/servers').length, 2)
        http.answer(http.pending('/api/servers')[1], passiveServers(passiveB)); await polling
        assert.equal(c.x.active.value.name, 'Secondary')
        assert.equal(c.x.active.value.seq, 2)
        assert.equal(c.x.loading.value, true, 'Passive completion must not finish a pending foreground list load')
        http.answer(old, passiveServers(passiveA))
        if (listRead) await listRead
        await watchVM.flush()
        assert.equal(c.x.groups.value.length, 1, 'A valid list reply must still populate the rows')
        assert.equal(c.x.groups.value[0].servers.map(s => s.fingerprint).join(','), 'abcdef01,abcdef02')
        assert.equal(c.x.subscriptions.value.length, 1)
        assertPassiveBadges(c, passiveB)
        assert.equal(c.x.loading.value, false)
        assert.equal(http.calls('/api/servers').length, origin === 'initial' ? 2 : 3)
        assert.equal(http.calls('/api/subscriptions').length, origin === 'initial' ? 1 : 2)
        assert.equal(http.requests.filter(r => r.method !== 'GET').length, 0)
      }, http => { if (origin === 'initial') http.hold('/api/servers') })
    })
  }

  await test('Task16 I16-F1 a monitor-triggered list reread fences the overlapping passive GET without losing refreshed rows', async () => {
    await withPassivePage(async (c, http) => {
      const changed = { ...watchVM.group, name: 'Updated synthetic', servers: [{ ...watchVM.primary, fingerprint: 'abcdef04' }, watchVM.group.servers[1]] }
      const active = { ...passiveB, seq: 3 }
      http.hold('/api/servers')
      http.set('/api/subscriptions', { subscriptions: [{ ...c.x.subscriptions.value[0], name: changed.name }] })
      http.set('/api/monitor', { ...watchVM.health, subscriptions: [{ ...watchVM.health.subscriptions[0], servers: [
        { ...watchVM.health.subscriptions[0].servers[0], fingerprint: 'abcdef04', latency_ms: 201 }, watchVM.health.subscriptions[0].servers[1],
      ] }] })
      const polling = passiveTimer(c)[1].fn(); await watchVM.flush()
      assert.equal(http.pending('/api/servers').length, 2, 'List mismatch must reread the list independently of the active-only poll')
      const [old, fresh] = http.pending('/api/servers')
      assert.equal(c.x.loading.value, true)
      assert.equal(c.x.healthOf(c.x.groups.value[0], 0, c.x.groups.value[0].servers[0]), null)
      http.answer(fresh, { subscriptions: [changed], active }); await watchVM.flush()
      assertPassiveBadges(c, active)
      assert.equal(c.x.groups.value[0].name, 'Updated synthetic')
      assert.equal(c.x.subscriptions.value[0].name, 'Updated synthetic')
      assert.equal(c.x.groups.value[0].servers[0].fingerprint, 'abcdef04')
      assert.equal(c.x.healthOf(c.x.groups.value[0], 0, c.x.groups.value[0].servers[0]).latency_ms, 201)
      assert.equal(c.x.loading.value, false)
      const before = visibleRows(c)
      http.answer(old, passiveServers(passiveA)); await polling
      assertPassiveBadges(c, active)
      assertSameRows(c, before)
      assert.equal(c.x.aliveText(watchVM.group.id), '2/2 alive')
      assert.equal(http.calls('/api/servers').length, 3)
      assert.equal(http.calls('/api/subscriptions').length, 2)
      assert.equal(http.calls('/api/monitor').length, 2)
      assert.equal(http.calls('/api/watch').length, 3)
      assert.equal(http.requests.filter(r => r.method !== 'GET').length, 0)
    })
  })

  for (const control of ['success', 'saved500']) {
    await test(`Task16 I16-F1 a newer ${control} control reread fences an older passive GET`, async () => {
      await withPassivePage(async (c, http) => {
        http.hold('/api/servers')
        const polling = passiveTimer(c)[1].fn(); await watchVM.flush()
        assert.equal(http.pending('/api/servers').length, 1)
        const old = http.pending('/api/servers')[0]
        const action = watchVM.click(c, 'Select', 1)
        const post = http.pending('/api/servers/active')[0]
        assert.equal(JSON.stringify(post.body), '{"subscription":"0a1b2c3d","index":1,"name":"Secondary","address":"198.51.100.2","port":8443}')
        if (control === 'saved500') http.reject(post, { response: { status: 500, data: { error: 'failed to restart xray: Synthetic saved selection' } } })
        else http.answer(post, { ok: true })
        await watchVM.flush()
        assert.equal(http.pending('/api/servers').length, 2)
        http.answer(http.pending('/api/servers')[1], passiveServers(passiveB)); await action
        assertPassiveBadges(c, passiveB)
        const before = visibleRows(c)
        http.answer(old, passiveServers(passiveA)); await polling
        assertPassiveBadges(c, passiveB)
        assertSameRows(c, before)
        assert.equal(c.x.busy.value, '')
        assert.equal(c.x.loading.value, false)
        assert.equal(c.x.error.value, '')
        assert.deepEqual(c.alerts, [control === 'saved500' ? 'Error: failed to restart xray: Synthetic saved selection' : 'Server selected: Synthetic / Secondary'])
        assert.equal(http.calls('/api/servers').length, 3)
        assert.equal(http.calls('/api/subscriptions').length, 2)
        assert.equal(http.calls('/api/servers/active', 'POST').length, 1)
        assert.equal(http.requests.filter(r => r.method !== 'GET').length, 1)
      })
    })
  }

  await test('Task16 I16-F1 an older control reread cannot overwrite a newer passive preferred return', async () => {
    await withPassivePage(async (c, http) => {
      http.hold('/api/servers')
      const action = watchVM.click(c, 'Select', 1)
      http.answer(http.pending('/api/servers/active')[0], { ok: true }); await watchVM.flush()
      assert.equal(http.pending('/api/servers').length, 1)
      const old = http.pending('/api/servers')[0]
      const polling = passiveTimer(c)[1].fn(); await watchVM.flush()
      assert.equal(http.pending('/api/servers').length, 2)
      const returned = { ...passiveA, seq: 3 }
      http.answer(http.pending('/api/servers')[1], passiveServers(returned)); await polling
      assertPassiveBadges(c, returned)
      assert.equal(c.x.busy.value, 'select:0a1b2c3d:1')
      assert.equal(c.x.loading.value, true)
      http.answer(old, passiveServers(passiveB)); await action
      assertPassiveBadges(c, returned)
      assert.equal(c.x.loading.value, false)
      assert.equal(c.x.busy.value, '')
      assert.equal(http.calls('/api/servers').length, 3)
      assert.equal(http.calls('/api/servers/active', 'POST').length, 1)
      assert.equal(http.requests.filter(r => r.method !== 'GET').length, 1)
    })
  })

  await test('Task16 I16-F1 passive null active clears both badges without replacing rows', async () => {
    await withPassivePage(async (c, http) => {
      const before = visibleRows(c), start = http.requests.length
      http.set('/api/servers', passiveServers(null))
      await c.fire(passiveTimer(c)[0])
      assertPassiveBadges(c, null)
      assertSameRows(c, before)
      assertPassiveReads(http, start)
      assert.equal(c.x.aliveText(watchVM.group.id), '2/2 alive')
    })
  })

  for (const departure of ['unmount', 'logout']) for (const outcome of ['success', 'GET500']) {
    await test(`Task16 I16-F1 late passive ${outcome} after ${departure} cannot publish restart reads or affect a fresh instance`, async () => {
      await withPassivePage(async (c, http) => {
        for (const url of ['/api/servers', '/api/monitor', '/api/watch']) http.hold(url)
        const polling = passiveTimer(c)[1].fn(); await watchVM.flush()
        for (const url of ['/api/servers', '/api/monitor', '/api/watch']) assert.equal(http.pending(url).length, 1)
        const callbacks = [...c.timers.values()].map(t => t.fn)
        if (departure === 'logout') {
          const logout = c.x.api.logout()
          http.answer(http.pending('/api/logout')[0], { ok: true }); await logout
        }
        c.unmount()
        const before = JSON.stringify({ active: c.x.active.value, monitor: c.x.monitor.value, watch: c.x.watch.value, error: c.x.error.value, loading: c.x.loading.value })
        const rows = visibleRows(c), start = http.requests.length
        for (const r of http.requests.filter(r => !r.settled)) {
          if (outcome === 'GET500') http.reject(r, passiveFailure)
          else http.answer(r, { '/api/servers': passiveServers(passiveB), '/api/monitor': { ...watchVM.health, state: 'stopped' }, '/api/watch': { ...watchVM.watch, pending_restore: true } }[r.url])
        }
        await polling
        callbacks.forEach(fn => fn())
        await c.x.pollStatus(); await c.x.load(); await c.x.loadMonitor(); await watchVM.flush()
        assert.equal(JSON.stringify({ active: c.x.active.value, monitor: c.x.monitor.value, watch: c.x.watch.value, error: c.x.error.value, loading: c.x.loading.value }), before)
        assertSameRows(c, rows)
        assertPassiveBadges(c, passiveA)
        assert.equal(http.requests.length, start)
        assert.equal(c.timers.size, 0)
        assert.deepEqual(c.alerts, [])
        assert.equal(http.calls('/api/servers/active', 'POST').length, 0)
        assert.equal(http.calls('/api/logout', 'POST').length, departure === 'logout' ? 1 : 0)
        assert.equal(http.calls('/api/login', 'POST').length, 0)
        assert.equal(http.calls('/api/version').length, 0)
        const freshHTTP = watchVM.makeHTTP(), fresh = watchVM.setupPage('Servers', freshHTTP)
        freshHTTP.set('/api/servers', passiveServers(passiveB))
        try {
          await fresh.mount()
          assertPassiveBadges(fresh, passiveB)
          assert.equal(freshHTTP.calls('/api/servers').length, 1)
          assert.equal(fresh.timers.size, 1)
          assertPassiveBadges(c, passiveA)
        } finally { fresh.unmount() }
      })
    })
  }
  console.log(`RESULT ${passes} PASS / ${failures} FAIL`)
  process.exitCode = failures ? 1 : 0
})()
