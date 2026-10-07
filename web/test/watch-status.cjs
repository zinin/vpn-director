const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')
const assert = require('node:assert/strict')
const ts = require('typescript')
const { compileTemplate } = require('vue/compiler-sfc')
const vueRuntime = require('vue')

const sourceRoot = path.join(__dirname, '..', 'src')
const compilerOptions = { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 }
const clone = value => JSON.parse(JSON.stringify(value))
const stamp = '2026-10-07T12:00:00Z'
const primary = { name: 'Primary', address: '192.0.2.1', port: 443, ips: ['192.0.2.1'], protocol: 'trojan', fingerprint: 'abcdef01' }
const secondary = { name: 'Secondary', address: '198.51.100.2', port: 8443, ips: ['198.51.100.2'], protocol: 'trojan', fingerprint: 'abcdef02' }
const group = { id: '0a1b2c3d', name: 'Synthetic', servers: [primary, secondary] }
const subscription = { id: '0a1b2c3d', name: 'Synthetic', host: 'subscription.example.test', static: false, servers: 2, added: stamp, refreshed: stamp }
const active = { subscription: '0a1b2c3d', name: 'Primary', address: '192.0.2.1', port: 443 }
const health = {
  state: 'ok', interval_seconds: 60, lag_seconds: 0,
  subscriptions: [{ id: '0a1b2c3d', alive: 2, total: 2, servers: [
    { index: 0, fingerprint: 'abcdef01', status: 'alive', latency_ms: 142, checked_at: stamp, since: stamp, next_at: '2026-10-07T12:01:00Z' },
    { index: 1, fingerprint: 'abcdef02', status: 'alive', latency_ms: 81, checked_at: stamp, since: stamp, next_at: '2026-10-07T12:01:00Z' },
  ] }],
}
const watch = { state: 'active', updated_at: stamp, message: '', action: '', committed_failover: false, pending_restore: false, notifications: { pending: 0 } }
const wanHealth = { ...health, state: 'wan_down' }
const queuedNotice = 'Check queued; waiting for WAN recovery.'

function deferred() {
  let resolve, reject
  const promise = new Promise((yes, no) => { resolve = yes; reject = no })
  return { promise, resolve, reject }
}

async function flush() {
  await new Promise(resolve => setImmediate(resolve))
}

function makeHTTP() {
  const requests = [], held = new Set(), errors = new Map()
  const data = new Map([
    ['/api/status', { output: 'Synthetic VPN Director: running' }],
    ['/api/ip', { ip: '203.0.113.10' }],
    ['/api/servers', { subscriptions: [group], active }],
    ['/api/subscriptions', { subscriptions: [subscription] }],
    ['/api/monitor', health], ['/api/watch', watch],
  ])
  function request(method, url, body, options) {
    const d = deferred()
    const r = { method, url, body, options, ...d, settled: false }
    requests.push(r)
    if (method === 'GET' && !held.has(url)) {
      r.settled = true
      if (errors.has(url)) d.reject(errors.get(url))
      else {
        assert.ok(data.has(url), 'Unexpected GET ' + url)
        d.resolve({ data: clone(data.get(url)) })
      }
    }
    return d.promise
  }
  const http = {
    get: (url, options) => request('GET', url, undefined, options),
    post: (url, body, options) => request('POST', url, body, options),
    delete: (url, options) => request('DELETE', url, undefined, options),
    interceptors: { response: { use: () => {} } },
  }
  return {
    http, requests,
    set: (url, value) => data.set(url, clone(value)),
    hold: url => held.add(url),
    fail: (url, error = Error('Synthetic socket unavailable')) => errors.set(url, error),
    recover: url => { held.delete(url); errors.delete(url) },
    calls: (url, method = 'GET') => requests.filter(r => r.url === url && r.method === method),
    pending: url => requests.filter(r => r.url === url && !r.settled),
    answer: (r, value) => { assert.ok(r && !r.settled, 'Expected an outstanding request'); r.settled = true; r.resolve({ data: clone(value) }) },
    reject: (r, error = Error('Synthetic socket unavailable')) => { assert.ok(r && !r.settled, 'Expected an outstanding request'); r.settled = true; r.reject(error) },
  }
}

function bindingNames(script) {
  const source = ts.createSourceFile('component.ts', script, ts.ScriptTarget.Latest, true, ts.ScriptKind.TS)
  const names = []
  function add(name) {
    if (ts.isIdentifier(name)) names.push(name.text)
    else for (const element of name.elements) if (element.name) add(element.name)
  }
  for (const statement of source.statements) {
    if (ts.isVariableStatement(statement)) for (const declaration of statement.declarationList.declarations) add(declaration.name)
    if (ts.isFunctionDeclaration(statement) && statement.name) add(statement.name)
    if (ts.isImportDeclaration(statement) && statement.importClause && !statement.importClause.isTypeOnly) {
      const clause = statement.importClause
      if (clause.name) add(clause.name)
      if (clause.namedBindings) {
        if (ts.isNamespaceImport(clause.namedBindings)) add(clause.namedBindings.name)
        else for (const element of clause.namedBindings.elements) if (!element.isTypeOnly) add(element.name)
      }
    }
  }
  return names
}

function createLoader(http, lifecycle, props = {}) {
  const cache = new Map()
  const vnode = (type, properties, children) => ({ type, props: properties ?? {}, children })
  const isRef = value => value && value.__v_isRef === true
  const unref = value => isRef(value) ? value.value : value
  const vue = {
    ...vueRuntime,
    ref: value => ({ __v_isRef: true, value }),
    computed: fn => ({ __v_isRef: true, get value() { return fn() } }),
    unref, isRef,
    onMounted: fn => lifecycle.mounted.push(fn), onUnmounted: fn => lifecycle.unmount.push(fn),
    openBlock: () => {}, createElementBlock: vnode, createElementVNode: vnode,
    createVNode: vnode, createBlock: vnode,
    createTextVNode: text => vnode('#text', {}, text), createCommentVNode: () => vnode('#comment', {}, ''),
    withDirectives: node => node,
    resolveComponent: name => {
      const component = lifecycle.bindings[name]
      assert.ok(component?.filename, 'Unresolved real component ' + name)
      return component
    },
  }
  const context = () => ({
    exports: {}, require: name => {
      if (name === 'vue') return vue
      if (name === 'axios') return { default: { create: () => http.http } }
      assert.ok(name.startsWith('.'), 'Unexpected module ' + name)
      const resolved = path.resolve(lifecycle.directory, name)
      return load(resolved.endsWith('.vue') ? resolved : resolved + '.ts')
    },
    Date, AbortController,
    defineProps: () => props,
    withDefaults: (value, defaults) => Object.assign({}, defaults, value),
    alert: message => lifecycle.alerts.push(message), confirm: () => true,
    setTimeout: (fn, ms) => lifecycle.schedule('timeout', fn, ms), clearTimeout: id => lifecycle.timers.delete(id),
    setInterval: (fn, ms) => lifecycle.schedule('interval', fn, ms), clearInterval: id => lifecycle.timers.delete(id),
  })
  function load(filename) {
    if (cache.has(filename)) return cache.get(filename)
    assert.ok(fs.existsSync(filename), 'Task 14 real source is missing: ' + filename)
    if (filename.endsWith('.vue')) {
      const module = { default: { filename } }
      cache.set(filename, module)
      return module
    }
    const text = fs.readFileSync(filename, 'utf8')
    const c = context(), previous = lifecycle.directory
    lifecycle.directory = path.dirname(filename)
    try { vm.runInNewContext(ts.transpileModule(text, { compilerOptions }).outputText, c, { filename }) }
    finally { lifecycle.directory = previous }
    cache.set(filename, c.exports)
    return c.exports
  }
  return { vue, context, load }
}

function instantiate(filename, http, props = {}) {
  const mounted = [], unmount = [], timers = new Map(), alerts = []
  let timer = 0
  const lifecycle = { mounted, unmount, timers, alerts, directory: path.dirname(filename), bindings: {},
    schedule: (kind, fn, ms) => { timers.set(++timer, { kind, fn, ms }); return timer } }
  assert.ok(fs.existsSync(filename), 'Task 14 real component is missing: ' + filename)
  const component = fs.readFileSync(filename, 'utf8')
  const script = component.match(/<script setup lang="ts">([\s\S]*?)<\/script>/)[1]
  const template = component.match(/<template>([\s\S]*)<\/template>/)[1]
  const loader = createLoader(http, lifecycle, props)
  const c = loader.context()
  const exposed = bindingNames(script)
  const source = script + '\nexport const __testBindings = {' + exposed.join(',') + '}'
  vm.runInNewContext(ts.transpileModule(source, { compilerOptions }).outputText, c, { filename })
  const x = c.exports.__testBindings
  lifecycle.bindings = x
  const compiled = compileTemplate({ source: template, filename, id: 'task14-vm' })
  assert.deepEqual(compiled.errors, [], 'Real Vue template must compile')
  const renderContext = { exports: {}, require: name => { assert.equal(name, 'vue'); return loader.vue } }
  vm.runInNewContext(ts.transpileModule(compiled.code, { compilerOptions }).outputText, renderContext, { filename: filename + ':template' })
  const scope = new Proxy({ ...props, ...x }, { get: (target, key) => loader.vue.unref(target[key]) })
  const render = () => renderContext.exports.render(scope, [])
  return {
    x, http, timers, alerts, filename, render,
    mount: async () => { mounted.forEach(fn => fn()); await flush() },
    unmount: () => unmount.forEach(fn => fn()),
    fire: async id => { const t = timers.get(id); assert.ok(t, 'Expected an actual component timer'); if (t.kind === 'timeout') timers.delete(id); const result = t.fn(); await flush(); return result },
    helper: loader.load(path.join(sourceRoot, 'watch.ts')),
  }
}

function setupPage(name, http = makeHTTP()) {
  return instantiate(path.join(sourceRoot, 'components', name + 'Tab.vue'), http)
}

function nodes(tree) {
  if (Array.isArray(tree)) return tree.flatMap(nodes)
  if (!tree || typeof tree !== 'object') return []
  return [tree, ...nodes(tree.children)]
}

function text(tree) {
  if (Array.isArray(tree)) return tree.map(text).join(' ')
  if (tree == null) return ''
  if (typeof tree !== 'object') return String(tree)
  if (tree.type === '#comment') return ''
  return text(tree.children).replace(/\s+/g, ' ').trim()
}

function watchNode(page) {
  const found = nodes(page.render()).filter(node => node.type?.filename?.endsWith('/WatchStatus.vue'))
  assert.equal(found.length, 1, 'Page must render the real shared WatchStatus once')
  return found[0]
}

function watchProps(page) {
  const props = watchNode(page).props
  assert.equal(typeof props.unavailable, 'boolean')
  assert.ok(Object.hasOwn(props, 'snapshot'))
  return props
}

function watchDisplay(page) {
  const node = watchNode(page)
  const component = instantiate(node.type.filename, page.http, node.props)
  const tree = component.render()
  assert.equal(nodes(tree).some(node => Object.hasOwn(node.props, 'innerHTML')), false, 'Status details must be text, not HTML')
  return text(tree)
}

function buttons(page) {
  return nodes(page.render()).filter(node => node.type === 'button')
}

function click(page, label, index = 0) {
  const button = buttons(page).filter(node => text(node) === label)[index]
  assert.ok(button, 'Missing real button ' + label)
  assert.ok(!button.props.disabled, 'Manual control disabled: ' + label)
  assert.equal(typeof button.props.onClick, 'function')
  return button.props.onClick({ preventDefault() {}, stopPropagation() {} })
}

function visibleValues(page) {
  const result = {}
  for (const name of ['status', 'ip', 'ipError', 'activeServer', 'activeLabel', 'serverError', 'serverLoaded', 'groups', 'subscriptions', 'active', 'monitor', 'monitorUnavailable', 'loading', 'error']) {
    if (page.x[name]?.__v_isRef) result[name] = page.x[name].value
  }
  const props = watchProps(page)
  return clone({ ...result, watch: props.snapshot, watchUnavailable: props.unavailable })
}

function assertActive(page) {
  assert.equal(watchProps(page).snapshot.state, 'active')
  assert.equal(watchProps(page).unavailable, false)
  assert.equal(page.helper.watchText(watchProps(page).snapshot, false), 'Automation: active')
  assert.match(watchDisplay(page), /Automation: active/)
}

function assertReads(http, start, urls) {
  const reads = http.requests.slice(start).filter(r => r.method === 'GET').map(r => r.url)
  for (const url of urls) assert.ok(reads.includes(url), 'Control must reload ' + url)
}

async function independent() {
  for (const name of ['Status', 'Servers']) {
    const http = makeHTTP()
    http.set('/api/monitor', { ...health, state: 'disabled' })
    const page = setupPage(name, http)
    await page.mount()
    assert.equal(page.helper.watchText(null, true), 'Automation: unavailable')
    assert.equal(page.helper.watchText(watch, true), 'Automation: unavailable')
    assert.doesNotMatch(page.helper.watchText(null, false), /active|stopped|incompatible|error/i)
    assertActive(page)
    assert.match(text(page.render()), /Monitoring: disabled in settings/)
    if (name === 'Status') {
      assert.equal(page.x.status.value, 'Synthetic VPN Director: running')
      assert.equal(page.x.ip.value, '203.0.113.10')
      assert.equal(page.x.activeLabel.value, 'Synthetic / Primary')
    } else {
      assert.equal(page.x.canCheck.value, false)
      assert.equal(page.x.isActive('0a1b2c3d', primary), true)
    }
    page.unmount()
  }
  for (const failing of ['/api/status', '/api/ip', '/api/servers', '/api/monitor', '/api/watch']) {
    const http = makeHTTP()
    http.fail(failing)
    const page = setupPage('Status', http)
    await page.mount()
    if (failing !== '/api/status') assert.equal(page.x.status.value, 'Synthetic VPN Director: running')
    else assert.match(page.x.status.value, /Error: Synthetic socket unavailable/)
    if (failing !== '/api/ip') assert.equal(page.x.ip.value, '203.0.113.10')
    else assert.equal(page.x.ipError.value, 'Synthetic socket unavailable')
    if (failing !== '/api/servers') assert.equal(page.x.activeLabel.value, 'Synthetic / Primary')
    else assert.equal(page.x.serverError.value, 'Synthetic socket unavailable')
    if (failing !== '/api/watch') assertActive(page)
    else {
      assert.equal(watchProps(page).unavailable, true)
      assert.match(watchDisplay(page), /unavailable/i)
      assert.ok(!buttons(page).find(b => text(b) === '■ Stop').props.disabled)
    }
    if (failing !== '/api/monitor') assert.match(text(page.render()), /Monitoring every 1 min/)
    else assert.match(text(page.render()), /Monitoring: unavailable/)
    page.unmount()
  }
  for (const held of ['/api/monitor', '/api/watch']) {
    const http = makeHTTP()
    http.hold(held)
    const page = setupPage('Status', http)
    await page.mount()
    assert.equal(http.pending(held).length, 1)
    assert.equal(page.x.status.value, 'Synthetic VPN Director: running', 'A pending IPC read must not block shell status')
    assert.equal(page.x.ip.value, '203.0.113.10')
    assert.equal(page.x.activeLabel.value, 'Synthetic / Primary')
    if (held === '/api/monitor') assertActive(page)
    else assert.match(text(page.render()), /Monitoring every 1 min/)
    page.unmount()
    http.answer(http.pending(held)[0], held === '/api/watch' ? watch : health)
    await flush()
  }
  const cases = [
    ['starting', '', /starting/i], ['active', '', /active/i], ['stopped', 'VPN Director is stopped', /stopped/i],
    ['incompatible', 'Bot compatibility is unconfirmed', /incompatible|waiting/i],
    ['error', 'Subscription automation configuration is unavailable', /error|unavailable/i],
    ['not_running', 'Subscription automation is not running', /not running/i],
  ]
  for (const [state, message, want] of cases) {
    const http = makeHTTP(), value = { ...watch, state, message }
    http.set('/api/watch', value)
    const page = setupPage('Status', http)
    await page.mount()
    assert.match(page.helper.watchText(value, false), want)
    assert.match(watchDisplay(page), want)
    if (message) assert.ok(watchDisplay(page).includes(message), 'Show the safe runtime explanation')
    page.unmount()
  }
  for (const action of ['checking', 'switching', 'fallback', 'refreshing', 'walking', 'returning', 'restoring']) {
    const http = makeHTTP()
    http.set('/api/watch', { ...watch, action })
    const page = setupPage('Servers', http)
    await page.mount()
    assert.ok(watchDisplay(page).toLowerCase().includes(action), 'Show the current action ' + action)
    page.unmount()
  }
  for (const name of ['Status', 'Servers']) {
    const http = makeHTTP()
    http.set('/api/watch', { ...watch, notifications: { pending: 2, storage_error: 'Notification storage is unavailable' }, committed_failover: true, pending_restore: false })
    const page = setupPage(name, http)
    await page.mount()
    assertActive(page)
    assert.match(watchDisplay(page), /(?:2.*pending|pending.*2)/i)
    assert.match(watchDisplay(page), /Notification storage is unavailable/)
    assert.match(watchDisplay(page), /committed failover|failover.*committed/i)
    assert.doesNotMatch(watchDisplay(page), /pending restore|restore pending/i)
    if (name === 'Status') assert.equal(page.x.activeLabel.value, 'Synthetic / Primary')
    else {
      assert.equal(page.x.isActive('0a1b2c3d', primary), true)
      assert.equal(page.x.aliveText('0a1b2c3d'), '2/2 alive')
      assert.equal(page.x.healthOf(group, 0, primary).status, 'alive')
      assert.equal(page.x.healthText(page.x.healthOf(group, 0, primary)), '● 142 ms')
    }
    http.set('/api/watch', { ...watch, pending_restore: true })
    await page.x[name === 'Status' ? 'loadStatus' : 'load']()
    await flush()
    assert.equal(watchProps(page).snapshot.committed_failover, false)
    assert.equal(watchProps(page).snapshot.pending_restore, true)
    assert.match(watchDisplay(page), /pending restore|restore pending/i)
    assert.doesNotMatch(watchDisplay(page), /committed failover|failover.*committed/i)
    page.unmount()
  }
  for (const name of ['Status', 'Servers']) {
    const http = makeHTTP()
    http.set('/api/servers', { subscriptions: null, active: null })
    http.set('/api/subscriptions', { subscriptions: null })
    http.set('/api/monitor', { ...health, subscriptions: null })
    const page = setupPage(name, http)
    await page.mount()
    assertActive(page)
    assert.match(text(page.render()), name === 'Status' ? /none selected yet/ : /No subscriptions yet.*No servers found/)
    assert.equal(http.requests.filter(r => r.method !== 'GET').length, 0)
    page.unmount()
  }
}

async function staleResponses() {
  for (const name of ['Status', 'Servers']) for (const newer of ['failure', 'recovery', 'stopped']) {
    const http = makeHTTP()
    http.hold('/api/watch')
    const page = setupPage(name, http)
    await page.mount()
    const old = http.pending('/api/watch')[0]
    if (name === 'Status') page.x.loadStatus()
    else {
      const polls = [...page.timers.entries()].filter(([, t]) => t.kind === 'interval')
      assert.equal(polls.length, 1, 'Watch must share the existing server poll')
      polls[0][1].fn()
    }
    await flush()
    assert.equal(http.pending('/api/watch').length, 2)
    const fresh = http.pending('/api/watch')[1]
    if (newer === 'failure') http.reject(fresh)
    else http.answer(fresh, newer === 'stopped' ? { ...watch, state: 'stopped' } : watch)
    await flush()
    if (newer === 'failure') {
      assert.equal(watchProps(page).unavailable, true)
      http.answer(old, watch)
    } else {
      assert.equal(watchProps(page).snapshot.state, newer === 'stopped' ? 'stopped' : 'active')
      if (newer === 'stopped') http.answer(old, watch)
      else http.reject(old, Error('Synthetic old socket failure'))
    }
    await flush()
    assert.equal(watchProps(page).unavailable, newer === 'failure')
    if (newer === 'failure') assert.match(watchDisplay(page), /unavailable/i)
    else assert.equal(watchProps(page).snapshot.state, newer === 'stopped' ? 'stopped' : 'active')
    assert.match(text(page.render()), /Monitoring every 1 min/)
    if (name === 'Status') assert.equal(page.x.activeLabel.value, 'Synthetic / Primary')
    else assert.equal(page.x.healthOf(group, 0, primary).status, 'alive')
    page.unmount()
  }
}

async function cancelsPolling() {
  for (const name of ['Status', 'Servers']) for (const outcome of ['success', 'failure']) {
    const http = makeHTTP()
    for (const url of ['/api/status', '/api/ip', '/api/servers', '/api/subscriptions', '/api/monitor', '/api/watch']) http.hold(url)
    const page = setupPage(name, http)
    await page.mount()
    const callbacks = [...page.timers.values()].map(t => t.fn)
    assert.equal(http.pending('/api/watch').length, 1)
    page.unmount()
    const before = visibleValues(page), calls = http.requests.length
    for (const r of http.requests.filter(r => !r.settled)) {
      const value = {
        '/api/status': { output: 'Synthetic late status' }, '/api/ip': { ip: '203.0.113.99' },
        '/api/servers': { subscriptions: [group], active }, '/api/subscriptions': { subscriptions: [subscription] },
        '/api/monitor': health, '/api/watch': watch,
      }[r.url]
      if (outcome === 'success') http.answer(r, value)
      else http.reject(r, Error('Synthetic late failure'))
    }
    await flush()
    assert.deepEqual(visibleValues(page), before, 'Unmount must prevent late visible state mutation')
    assert.equal(page.timers.size, 0)
    for (const callback of callbacks) callback()
    await page.x[name === 'Status' ? 'loadStatus' : 'load']()
    await flush()
    assert.equal(http.requests.length, calls, 'Unmounted callbacks must not poll or reload')
    assert.equal(page.timers.size, 0)
    const freshHTTP = makeHTTP(), fresh = setupPage(name, freshHTTP)
    await fresh.mount()
    assertActive(fresh)
    assert.equal(freshHTTP.calls('/api/watch').length, 1)
    fresh.unmount()
    assert.equal(fresh.timers.size, 0)
  }
  for (const [name, label, url] of [['Status', '▶ Apply', '/api/apply'], ['Servers', 'Select', '/api/servers/active'], ['Servers', '⟳ Refresh all', '/api/subscriptions/refresh']]) {
    const http = makeHTTP(), page = setupPage(name, http)
    await page.mount()
    const action = click(page, label)
    assert.equal(http.pending(url).length, 1)
    page.unmount()
    const before = visibleValues(page), calls = http.requests.length
    http.answer(http.pending(url)[0], url === '/api/subscriptions/refresh' ? { results: [] } : { ok: true })
    await action
    await flush()
    assert.deepEqual(visibleValues(page), before)
    assert.equal(http.requests.length, calls, 'Late control completion must not restart reads')
    assert.equal(page.timers.size, 0)
  }
}

async function sharedControls() {
  const http = makeHTTP()
  let status = setupPage('Status', http)
  await status.mount()
  assertActive(status)
  for (const [label, url, state, output] of [
    ['■ Stop', '/api/stop', 'stopped', 'Synthetic VPN Director: stopped'],
    ['▶ Apply', '/api/apply', 'active', 'Synthetic VPN Director: applied'],
    ['↻ Restart', '/api/restart', 'active', 'Synthetic VPN Director: restarted'],
  ]) {
    const start = http.requests.length
    const action = click(status, label)
    assert.equal(http.pending(url).length, 1)
    assert.ok(buttons(status).filter(b => /Apply|Stop|Restart/.test(text(b)) || text(b) === '...').every(b => b.props.disabled))
    http.set('/api/status', { output })
    http.set('/api/monitor', state === 'stopped' ? { ...health, state: 'stopped' } : health)
    http.set('/api/watch', { ...watch, state })
    http.answer(http.pending(url)[0], { ok: true })
    await action
    await flush()
    assert.equal(status.x.status.value, output)
    assert.equal(status.x.actionLoading.value, '')
    assert.equal(status.x.activeLabel.value, 'Synthetic / Primary')
    assert.equal(watchProps(status).snapshot.state, state)
    assert.equal(status.helper.watchText(watchProps(status).snapshot, false), state === 'stopped' ? 'Automation: stopped' : 'Automation: active')
    assert.match(watchDisplay(status), state === 'stopped' ? /Automation: stopped/ : /Automation: active/)
    assertReads(http, start, ['/api/status', '/api/ip', '/api/servers', '/api/monitor', '/api/watch'])
    assert.equal(http.requests.slice(start).filter(r => r.method !== 'GET').length, 1)
    status.unmount()
    const servers = setupPage('Servers', http)
    await servers.mount()
    assert.equal(watchProps(servers).snapshot.state, state)
    assert.equal(servers.helper.watchText(watchProps(servers).snapshot, false), state === 'stopped' ? 'Automation: stopped' : 'Automation: active')
    assert.match(watchDisplay(servers), state === 'stopped' ? /Automation: stopped/ : /Automation: active/)
    assert.equal(servers.x.isActive('0a1b2c3d', primary), true)
    servers.unmount()
    status = setupPage('Status', http)
    await status.mount()
  }
  http.set('/api/ip', { ip: '203.0.113.11' })
  http.set('/api/watch', { ...watch, pending_restore: true })
  const beforeRefresh = http.requests.length
  await click(status, '⟳ Refresh')
  await flush()
  assert.equal(status.x.ip.value, '203.0.113.11')
  assert.equal(watchProps(status).snapshot.pending_restore, true)
  assert.match(watchDisplay(status), /pending restore|restore pending/i)
  assertReads(http, beforeRefresh, ['/api/status', '/api/ip', '/api/servers', '/api/monitor', '/api/watch'])
  assert.equal(http.requests.slice(beforeRefresh).filter(r => r.method !== 'GET').length, 0)
  status.unmount()
  const servers = setupPage('Servers', http)
  await servers.mount()
  assertActive(servers)
  assert.equal(watchProps(servers).snapshot.pending_restore, true)
  assert.match(watchDisplay(servers), /pending restore|restore pending/i)
  for (let selection = 0; selection < 2; selection++) {
    const start = http.requests.length
    const action = click(servers, 'Select', 1)
    const post = http.pending('/api/servers/active')[0]
    assert.equal(JSON.stringify(post.body), JSON.stringify({ subscription: '0a1b2c3d', index: 1, name: 'Secondary', address: '198.51.100.2', port: 8443 }))
    assert.equal(servers.x.busy.value, 'select:0a1b2c3d:1')
    http.set('/api/servers', { subscriptions: [group], active: { subscription: '0a1b2c3d', name: 'Secondary', address: '198.51.100.2', port: 8443 } })
    http.set('/api/watch', watch)
    http.answer(post, { ok: true })
    await action
    await flush()
    assertActive(servers)
    assert.equal(servers.x.isActive('0a1b2c3d', primary), false)
    assert.equal(servers.x.isActive('0a1b2c3d', secondary), true)
    assert.equal(nodes(servers.render()).filter(node => node.type === 'span' && text(node) === 'Active').length, 1)
    assert.match(text(servers.render()), /Secondary Active/)
    assert.equal(servers.x.busy.value, '')
    assertReads(http, start, ['/api/servers', '/api/subscriptions', '/api/monitor', '/api/watch'])
    assert.equal(http.requests.slice(start).filter(r => r.method !== 'GET').length, 1, 'Same-server selection must not be dropped or duplicated')
  }
  for (const all of [false, true]) {
    const start = http.requests.length
    const button = all ? buttons(servers).find(b => text(b) === '⟳ Refresh all') : buttons(servers).find(b => b.props.title === 'Refresh')
    assert.ok(button && !button.props.disabled)
    const action = button.props.onClick()
    const post = http.pending('/api/subscriptions/refresh')[0]
    assert.equal(JSON.stringify(post.options.params), all ? '{}' : '{"id":"0a1b2c3d"}')
    const changedGroup = { ...group, name: 'Refreshed synthetic' }
    http.set('/api/servers', { subscriptions: [changedGroup], active: { subscription: '0a1b2c3d', name: 'Secondary', address: '198.51.100.2', port: 8443 } })
    http.set('/api/subscriptions', { subscriptions: [{ ...subscription, name: 'Refreshed synthetic' }] })
    http.set('/api/watch', { ...watch, notifications: { pending: 2 } })
    http.answer(post, { results: [{ id: '0a1b2c3d', name: 'Refreshed synthetic', existed: true, summary: 'Synthetic: Imported 2 of 2 servers' }] })
    await action
    await flush()
    assert.equal(servers.x.groups.value[0].name, 'Refreshed synthetic')
    assert.equal(servers.x.subscriptions.value[0].name, 'Refreshed synthetic')
    assert.equal(servers.x.summaries.value[0], 'Synthetic: Imported 2 of 2 servers')
    assert.equal(watchProps(servers).snapshot.notifications.pending, 2)
    assert.equal(servers.x.aliveText('0a1b2c3d'), '2/2 alive')
    assertReads(http, start, ['/api/servers', '/api/subscriptions', '/api/monitor', '/api/watch'])
    assert.equal(http.requests.slice(start).filter(r => r.method !== 'GET').length, 1)
  }
  servers.unmount()
  status = setupPage('Status', http)
  await status.mount()
  assert.equal(status.x.activeLabel.value, 'Refreshed synthetic / Secondary')
  assert.equal(watchProps(status).snapshot.notifications.pending, 2)
  assertActive(status)
  assert.match(watchDisplay(status), /(?:2.*pending|pending.*2)/i)
  status.unmount()
  for (const name of ['Status', 'Servers']) {
    const down = makeHTTP()
    down.fail('/api/watch')
    const page = setupPage(name, down)
    await page.mount()
    assert.equal(watchProps(page).unavailable, true)
    const controls = name === 'Status'
      ? [['■ Stop', '/api/stop'], ['▶ Apply', '/api/apply'], ['↻ Restart', '/api/restart']]
      : [['Select', '/api/servers/active']]
    for (const [label, url] of controls) {
      const start = down.requests.length, action = click(page, label)
      assert.equal(down.pending(url).length, 1)
      down.answer(down.pending(url)[0], { ok: true })
      await action
      await flush()
      assert.equal(watchProps(page).unavailable, true)
      assert.equal(down.requests.slice(start).filter(r => r.method !== 'GET').length, 1)
    }
    if (name === 'Status') assert.equal(page.x.activeLabel.value, 'Synthetic / Primary')
    else {
      assert.equal(page.x.isActive('0a1b2c3d', primary), true)
      assert.equal(page.x.healthOf(group, 0, primary).status, 'alive')
      assert.equal(page.x.canCheck.value, true)
    }
    down.recover('/api/watch')
    await page.x[name === 'Status' ? 'loadStatus' : 'load']()
    await flush()
    assertActive(page)
    page.unmount()
  }
}

async function requestTimeout() {
  const http = makeHTTP()
  const lifecycle = { directory: sourceRoot, bindings: {}, mounted: [], unmount: [], timers: new Map(), alerts: [] }
  const loader = createLoader(http, lifecycle)
  const api = loader.load(path.join(sourceRoot, 'api.ts')).default
  assert.equal(typeof api.getWatch, 'function', 'Real api.getWatch must exist')
  const response = await api.getWatch()
  assert.equal(response.data.state, 'active')
  assert.equal(http.requests.length, 1)
  assert.equal(http.requests[0].method, 'GET')
  assert.equal(http.requests[0].url, '/api/watch')
  assert.equal(http.requests[0].options.timeout, 8000)
  assert.equal(http.requests.filter(r => r.method !== 'GET').length, 0)
}

function loadWatchModule() {
  const lifecycle = { directory: sourceRoot, bindings: {}, mounted: [], unmount: [], timers: new Map(), alerts: [] }
  return createLoader(makeHTTP(), lifecycle).load(path.join(sourceRoot, 'watch.ts'))
}

module.exports = { makeHTTP, setupPage, flush, text, watchProps, watchDisplay, buttons, click, group, primary, health, wanHealth, watch, queuedNotice, loadWatchModule }

if (require.main === module) {
  ;(async () => {
    let passes = 0, failures = 0
    for (const [name, fn] of [
      ['watch state is independent', independent],
      ['stale response cannot replace current state', staleResponses],
      ['unmount cancels polling', cancelsPolling],
      ['shared pages agree after controls', sharedControls],
      ['watch request has an 8000ms timeout', requestTimeout],
    ]) {
      try { await fn(); passes++; console.log('PASS ' + name) }
      catch (e) { failures++; console.error('FAIL ' + name + ': ' + e.message) }
    }
    console.log(`RESULT ${passes} PASS / ${failures} FAIL`)
    process.exitCode = failures ? 1 : 0
  })()
}
