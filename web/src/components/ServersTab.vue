<script setup lang="ts">
import { ref, computed, onMounted, onUnmounted } from 'vue'
import api from '../api'
import { monitorText } from '../monitor'
import WatchStatus from './WatchStatus.vue'
import type {
  ActiveServer,
  MonitorResponse,
  Server,
  ServerHealth,
  Subscription,
  SubscriptionServers,
  WatchResponse,
} from '../types'

const subscriptions = ref<Subscription[]>([])
const groups = ref<SubscriptionServers[]>([])
const active = ref<ActiveServer | null>(null)
const loading = ref(false)
// What is running: 'add', 'refresh-all', 'refresh:<id>', 'rename:<id>',
// 'delete:<id>', 'select:<id>:<index>'. One thing at a time.
const busy = ref('')
const error = ref('')
const addUrl = ref('')
const addName = ref('')
const renaming = ref('')
const renameText = ref('')
// What the last add or refresh came to, a line per subscription.
const summaries = ref<string[]>([])
// The server monitor's answer, polled while the tab is open.
const monitor = ref<MonitorResponse | null>(null)
const monitorUnavailable = ref(false)
const watch = ref<WatchResponse | null>(null)
const watchUnavailable = ref(false)
// The check being sent: 'all' or '<subscription>:<index>'.
const checking = ref('')
const checkNotice = ref('')
let checkNoticeGeneration = 0
let poll: ReturnType<typeof setInterval> | undefined
let recheck: ReturnType<typeof setTimeout> | undefined
let unmounted = false
let monitorRequest = 0
let watchRequest = 0
let listRequest = 0
// Active reads share a generation without invalidating list publication.
let activeRequest = 0
// Subscription reads share a generation: an older answer never replaces a newer list.
let subscriptionsRequest = 0
let listLoaded = false
let checkRequest = 0

function errorText(e: any): string {
  return e?.response?.data?.error || e?.message || 'unknown error'
}

async function load() {
  if (unmounted) return
  const request = ++listRequest
  const activeGeneration = ++activeRequest
  const subscriptionsGeneration = ++subscriptionsRequest
  loading.value = true
  error.value = ''
  const watchRead = loadWatch()
  try {
    const [subsRes, serversRes] = await Promise.all([api.getSubscriptions(), api.getServers()])
    if (unmounted || request !== listRequest) return
    if (subscriptionsGeneration === subscriptionsRequest) subscriptions.value = subsRes.data.subscriptions ?? []
    groups.value = serversRes.data.subscriptions ?? []
    if (activeGeneration === activeRequest) active.value = serversRes.data.active ?? null
    listLoaded = true
  } catch (e: any) {
    if (unmounted || request !== listRequest) return
    error.value = errorText(e)
  } finally {
    if (!unmounted && request === listRequest) loading.value = false
  }
  await watchRead
}

async function loadActive() {
  if (unmounted) return
  const request = ++activeRequest
  try {
    const resp = await api.getServers()
    if (unmounted || request !== activeRequest) return
    active.value = resp.data.active ?? null
  } catch {
    return
  }
}

// watchd's periodic refresh records and clears a subscription's error and moves
// its refreshed without changing a server's fingerprint: the table follows them
// on every poll, not only when the server list moves.
async function loadSubscriptions() {
  if (unmounted || !listLoaded) return
  const request = ++subscriptionsRequest
  try {
    const resp = await api.getSubscriptions()
    if (unmounted || request !== subscriptionsRequest) return
    subscriptions.value = resp.data.subscriptions ?? []
  } catch {
    return
  }
}

async function loadWatch() {
  if (unmounted) return
  const request = ++watchRequest
  try {
    const resp = await api.getWatch()
    if (unmounted || request !== watchRequest) return
    watch.value = resp.data
    watchUnavailable.value = false
  } catch {
    if (unmounted || request !== watchRequest) return
    watch.value = null
    watchUnavailable.value = true
  }
}

// "2 h ago" for a time the router wrote in UTC, and "never" for one before
// 2000: a file made by hand can lack the time, and Go answers its zero time,
// 0001-01-01, for it.
function ago(iso: string): string {
  const t = Date.parse(iso)
  if (isNaN(t)) return ''
  if (t < Date.UTC(2000, 0, 1)) return 'never'
  const s = Math.max(0, Math.round((Date.now() - t) / 1000))
  if (s < 60) return 'just now'
  if (s < 3600) return `${Math.floor(s / 60)} min ago`
  if (s < 86400) return `${Math.floor(s / 3600)} h ago`
  return `${Math.floor(s / 86400)} d ago`
}

// The subscription and the name as well as the endpoint: two subscriptions
// can both name a server Germany-1, and eight servers of one can share an
// address:port.
function isActive(sub: string, server: Server): boolean {
  const a = active.value
  return (
    a !== null &&
    a.subscription === sub &&
    a.name === server.name &&
    a.address === server.address &&
    a.port === server.port
  )
}

function runsFrom(sub: string): boolean {
  return active.value?.subscription === sub
}

// "in 40 s", "in 5 min" or "now" for a time the router wrote; nothing for
// Go's zero time.
function until(iso: string): string {
  const t = Date.parse(iso)
  if (isNaN(t) || t < Date.UTC(2000, 0, 1)) return ''
  const s = Math.round((t - Date.now()) / 1000)
  if (s <= 0) return 'now'
  if (s < 60) return `in ${s} s`
  return `in ${Math.round(s / 60)} min`
}

async function loadMonitor() {
  if (unmounted) return
  const request = ++monitorRequest
  try {
    const resp = await api.getMonitor()
    if (unmounted || request !== monitorRequest) return
    monitorUnavailable.value = false
    monitor.value = resp.data
    if (monitor.value?.state !== 'wan_down') clearCheckNotice()
    // The first monitor reply can precede the initial server list.
    if (listLoaded && listChanged()) await load()
  } catch {
    if (unmounted || request !== monitorRequest) return
    monitorUnavailable.value = true
    monitor.value = null
    clearCheckNotice()
  }
}

// The monitor's rows go by index; a count or a fingerprint that differs from
// the page's means a refresh moved the list since the page loaded it.
function listChanged(): boolean {
  const subs = monitor.value?.subscriptions ?? []
  if (subs.length !== groups.value.length) return true
  for (const group of groups.value) {
    const rows = subs.find((s) => s.id === group.id)?.servers ?? []
    const servers = group.servers ?? []
    if (rows.length !== servers.length) return true
    if (servers.some((server, i) => rows[i].fingerprint !== server.fingerprint)) return true
  }
  return false
}

// The status of a server as the page shows it, never another server's.
function healthOf(group: SubscriptionServers, idx: number, server: Server): ServerHealth | null {
  const row = monitor.value?.subscriptions?.find((s) => s.id === group.id)?.servers?.[idx]
  return row && row.fingerprint === server.fingerprint ? row : null
}

function healthText(h: ServerHealth | null): string {
  switch (h?.status) {
    case 'alive':
      return `● ${h.latency_ms} ms`
    case 'dead':
      return '● down'
    case 'rejected':
      return 'rejected'
    default:
      return '● —'
  }
}

function healthTitle(h: ServerHealth | null): string {
  if (!h || h.status === 'unknown') return 'Not checked yet'
  if (h.status === 'rejected') return h.error || 'Xray refused this server'
  const lines = [`Checked ${ago(h.checked_at)}`, `${h.status === 'alive' ? 'Alive' : 'Down'} since ${ago(h.since)}`]
  if (h.error) lines.push(`Error: ${h.error}`)
  const next = until(h.next_at)
  if (next) lines.push(`Next check ${next}`)
  return lines.join('\n')
}

function aliveText(groupId: string): string {
  const m = monitor.value
  if (!m || m.state === 'not_running') return ''
  const sub = m.subscriptions?.find((s) => s.id === groupId)
  const group = groups.value.find((g) => g.id === groupId)
  if (!sub || !group) return ''
  const servers = group.servers ?? []
  const rows = sub.servers ?? []
  if (sub.total !== servers.length || rows.length !== servers.length) return ''
  if (servers.some((server, i) => rows[i].index !== i || rows[i].fingerprint !== server.fingerprint)) return ''
  return `${sub.alive}/${sub.total} alive`
}

const monitorLine = computed(() => monitorText(monitor.value, monitorUnavailable.value))

const canCheck = computed(() => monitor.value?.state === 'ok' || monitor.value?.state === 'wan_down')

// Clearing the notice also invalidates a check whose POST is still pending.
function clearCheckNotice() {
  checkNotice.value = ''
  return ++checkNoticeGeneration
}

// A check answers within seconds: the page looks again before the next poll.
async function sendCheck(what: string, fn: () => Promise<{ data: { queued: number } }>) {
  if (unmounted) return
  const request = ++checkRequest
  const noticeGeneration = clearCheckNotice()
  checking.value = what
  try {
    const resp = await fn()
    if (!unmounted) {
      if (noticeGeneration === checkNoticeGeneration && monitor.value?.state === 'wan_down' && resp.data.queued > 0) {
        checkNotice.value = 'Check queued; waiting for WAN recovery.'
      }
      clearTimeout(recheck)
      recheck = setTimeout(loadMonitor, 3000)
    }
  } catch (e: any) {
    if (unmounted || request !== checkRequest) return
    if (e.response?.status === 409 && e.response?.data?.error === 'server list changed') {
      await load()
    } else {
      alert('Error: ' + errorText(e))
    }
  } finally {
    if (!unmounted && request === checkRequest) checking.value = ''
  }
}

function checkServer(group: SubscriptionServers, idx: number, server: Server) {
  return sendCheck(`${group.id}:${idx}`, () => api.checkServer(group.id, idx, server.fingerprint))
}

function checkAll() {
  return sendCheck('all', () => api.checkAllServers())
}

async function run(what: string, fn: () => Promise<void>) {
  if (unmounted || busy.value) return
  busy.value = what
  try {
    await fn()
  } catch (e: any) {
    if (unmounted) return
    alert('Error: ' + errorText(e))
    // A failure can still have changed the list: "saved, but" and "deleted,
    // but" answers, or a 404 for a subscription deleted meanwhile.
    await load()
    await loadMonitor()
  } finally {
    if (!unmounted) busy.value = ''
  }
}

function addSubscription() {
  if (!addUrl.value.trim()) return
  return run('add', async () => {
    summaries.value = []
    const resp = await api.addSubscription(addUrl.value.trim(), addName.value.trim())
    if (unmounted) return
    summaries.value = resp.data.existed
      ? [resp.data.summary, 'The link was saved already; its list was refreshed.']
      : [resp.data.summary]
    addUrl.value = ''
    addName.value = ''
    await load()
    await loadMonitor()
  })
}

function refresh(id?: string) {
  return run(id ? 'refresh:' + id : 'refresh-all', async () => {
    summaries.value = []
    const resp = await api.refreshSubscription(id)
    if (unmounted) return
    summaries.value = (resp.data.results ?? []).map((r) => r.summary)
    await load()
    await loadMonitor()
  })
}

function startRename(sub: Subscription) {
  if (unmounted || busy.value) return
  renaming.value = sub.id
  renameText.value = sub.name
}

function saveRename(sub: Subscription) {
  // Enter reaches here while the ✓ button is disabled.
  if (unmounted || busy.value) return
  return run('rename:' + sub.id, async () => {
    await api.renameSubscription(sub.id, renameText.value.trim())
    if (unmounted) return
    renaming.value = ''
    await load()
    await loadMonitor()
  })
}

function remove(sub: Subscription) {
  if (unmounted || busy.value) return
  const warn = runsFrom(sub.id)
    ? '\n\nThe running Xray server comes from it. Xray keeps running it until you select another.'
    : ''
  if (!confirm(`Delete the subscription ${sub.name} and its ${sub.servers} servers?${warn}`)) return
  return run('delete:' + sub.id, async () => {
    const resp = await api.deleteSubscription(sub.id)
    if (unmounted) return
    if (resp.data.active_removed) {
      alert('The running server is no longer in any subscription. Select another server.')
    }
    await load()
    await loadMonitor()
  })
}

async function selectServer(group: SubscriptionServers, index: number) {
  if (unmounted || busy.value) return
  const server = group.servers?.[index]
  if (!server) return
  busy.value = `select:${group.id}:${index}`
  try {
    await api.selectServer(group.id, index, server)
    if (unmounted) return
    alert(`Server selected: ${group.name} / ${server.name}`)
    await load()
    await loadMonitor()
  } catch (e: any) {
    if (unmounted) return
    if (e.response?.status === 409) {
      // A refresh on the router - the bot's subscription watch, an import in
      // another tab - moved the list since it was shown.
      alert('The server list changed; it has been reloaded. Select the server again.')
      await load()
      await loadMonitor()
    } else {
      alert('Error: ' + errorText(e))
      await load()
      await loadMonitor()
    }
  } finally {
    if (!unmounted) busy.value = ''
  }
}

async function pollStatus() {
  if (unmounted) return
  await Promise.all([loadMonitor(), loadWatch(), loadActive(), loadSubscriptions()])
}

onMounted(() => {
  load()
  loadMonitor()
  poll = setInterval(pollStatus, 15000)
})

onUnmounted(() => {
  unmounted = true
  listRequest++
  activeRequest++
  subscriptionsRequest++
  monitorRequest++
  watchRequest++
  checkRequest++
  clearCheckNotice()
  clearInterval(poll)
  clearTimeout(recheck)
})
</script>

<template>
  <div class="card">
    <div class="card-title">Subscriptions</div>

    <div v-if="subscriptions.length > 0" class="servers-table">
      <table>
        <thead>
          <tr>
            <th>Name</th>
            <th>Host</th>
            <th>Servers</th>
            <th>Changed</th>
            <th>Status</th>
            <th>Actions</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="sub in subscriptions" :key="sub.id">
            <td>
              <template v-if="renaming === sub.id">
                <input
                  v-model="renameText"
                  type="text"
                  maxlength="32"
                  style="width: 10rem;"
                  @keyup.enter="saveRename(sub)"
                  @keyup.esc="renaming = ''"
                />
                <button class="btn btn-green" :disabled="!!busy" @click="saveRename(sub)">✓</button>
                <button class="btn btn-blue" @click="renaming = ''">✕</button>
              </template>
              <template v-else>{{ sub.name }}</template>
            </td>
            <td>{{ sub.static ? 'static list' : sub.host }}</td>
            <td>{{ sub.servers }}</td>
            <td>{{ ago(sub.refreshed) }}</td>
            <td>
              <span v-if="sub.error" class="badge badge-red" :title="sub.error">{{ sub.error }}</span>
              <span v-else class="badge badge-green">OK</span>
            </td>
            <td style="white-space: nowrap;">
              <button
                class="btn btn-blue"
                :disabled="!!busy || sub.static"
                :title="sub.static ? 'A static list has no link to refresh' : 'Refresh'"
                @click="refresh(sub.id)"
              >
                {{ busy === 'refresh:' + sub.id ? '...' : '⟳' }}
              </button>
              <button class="btn btn-blue" :disabled="!!busy" title="Rename" @click="startRename(sub)">✎</button>
              <button class="btn btn-red" :disabled="!!busy" title="Delete" @click="remove(sub)">
                {{ busy === 'delete:' + sub.id ? '...' : '🗑' }}
              </button>
            </td>
          </tr>
        </tbody>
      </table>
    </div>
    <p v-else-if="!loading && !error" style="color: #999; font-size: 0.875rem;">
      No subscriptions yet. Add one below.
    </p>

    <div class="actions" style="display: flex; gap: 0.5rem; align-items: center; flex-wrap: wrap; margin-top: 0.75rem;">
      <input v-model="addUrl" type="text" placeholder="https://... subscription URL" style="flex: 2; min-width: 200px;" />
      <input v-model="addName" type="text" maxlength="32" placeholder="Name (optional)" style="flex: 1; min-width: 120px;" />
      <button class="btn btn-primary" :disabled="!!busy || !addUrl.trim()" @click="addSubscription">
        {{ busy === 'add' ? '...' : '+ Add' }}
      </button>
      <button
        class="btn btn-blue"
        :disabled="!!busy || subscriptions.every((s) => s.static)"
        @click="refresh()"
      >
        {{ busy === 'refresh-all' ? '...' : '⟳ Refresh all' }}
      </button>
      <button class="btn btn-blue" :disabled="loading" @click="load">
        {{ loading ? '...' : '↻ Reload' }}
      </button>
    </div>

    <p v-if="error" class="error-msg">{{ error }}</p>
    <p v-for="(line, i) in summaries" :key="i" style="font-size: 0.875rem; margin: 0.25rem 0;">{{ line }}</p>
  </div>

  <div class="card">
    <div class="card-title">Servers</div>
    <p v-if="monitorLine" style="font-size: 0.875rem; margin: 0 0 0.75rem;">
      {{ monitorLine }}
      <button
        v-if="canCheck"
        class="btn btn-blue"
        style="margin-left: 0.5rem;"
        :disabled="checking !== ''"
        @click="checkAll"
      >
        {{ checking === 'all' ? '...' : 'Check all now' }}
      </button>
    </p>
    <p v-if="checkNotice" class="kv-label" style="margin: 0 0 0.75rem;" role="status">{{ checkNotice }}</p>
    <WatchStatus :snapshot="watch" :unavailable="watchUnavailable" style="margin-bottom: 0.75rem;" />
    <details
      v-for="group in groups"
      :key="group.id"
      :open="runsFrom(group.id) || groups.length === 1"
      style="margin-bottom: 0.75rem;"
    >
      <summary style="cursor: pointer; font-weight: 600;">
        {{ group.name }} — {{ (group.servers ?? []).length }} servers<template v-if="aliveText(group.id)">, {{ aliveText(group.id) }}</template>
        <span v-if="runsFrom(group.id)" class="badge badge-green" style="margin-left: 0.5rem;">running</span>
      </summary>
      <div class="servers-table">
        <table>
          <thead>
            <tr>
              <th>#</th>
              <th>Name</th>
              <th>Address</th>
              <th>Port</th>
              <th>Protocol</th>
              <th>Health</th>
              <th>Action</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="(server, idx) in group.servers ?? []" :key="idx">
              <td>{{ idx + 1 }}</td>
              <td>
                {{ server.name }}
                <span v-if="isActive(group.id, server)" class="badge badge-green" style="margin-left: 0.5rem;">Active</span>
              </td>
              <td>{{ server.address }}</td>
              <td>{{ server.port }}</td>
              <td>{{ server.protocol }}</td>
              <td
                :class="'health-' + (healthOf(group, idx, server)?.status ?? 'unknown')"
                :title="healthTitle(healthOf(group, idx, server))"
                style="white-space: nowrap;"
              >
                {{ healthText(healthOf(group, idx, server)) }}
                <button
                  v-if="canCheck"
                  class="btn btn-blue"
                  style="padding: 0 0.4rem; margin-left: 0.4rem;"
                  :disabled="checking !== ''"
                  title="Check now"
                  @click="checkServer(group, idx, server)"
                >
                  {{ checking === `${group.id}:${idx}` ? '...' : '↻' }}
                </button>
              </td>
              <td>
                <button class="btn btn-green" :disabled="!!busy" @click="selectServer(group, idx)">
                  {{ busy === `select:${group.id}:${idx}` ? '...' : 'Select' }}
                </button>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </details>
    <p v-if="groups.length === 0 && !loading && !error" style="color: #999; font-size: 0.875rem;">
      No servers found. Add a subscription to get started.
    </p>
  </div>
</template>
