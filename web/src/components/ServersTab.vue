<script setup lang="ts">
import { ref, onMounted } from 'vue'
import api from '../api'
import type { ActiveServer, Server, Subscription, SubscriptionServers } from '../types'

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

function errorText(e: any): string {
  return e?.response?.data?.error || e?.message || 'unknown error'
}

async function load() {
  loading.value = true
  error.value = ''
  try {
    const [subsRes, serversRes] = await Promise.all([api.getSubscriptions(), api.getServers()])
    subscriptions.value = subsRes.data.subscriptions ?? []
    groups.value = serversRes.data.subscriptions ?? []
    active.value = serversRes.data.active ?? null
  } catch (e: any) {
    error.value = errorText(e)
  } finally {
    loading.value = false
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

async function run(what: string, fn: () => Promise<void>) {
  busy.value = what
  try {
    await fn()
  } catch (e: any) {
    alert('Error: ' + errorText(e))
    // A failure can still have changed the list: "saved, but" and "deleted,
    // but" answers, or a 404 for a subscription deleted meanwhile.
    await load()
  } finally {
    busy.value = ''
  }
}

function addSubscription() {
  if (!addUrl.value.trim()) return
  return run('add', async () => {
    summaries.value = []
    const resp = await api.addSubscription(addUrl.value.trim(), addName.value.trim())
    summaries.value = resp.data.existed
      ? [resp.data.summary, 'The link was saved already; its list was refreshed.']
      : [resp.data.summary]
    addUrl.value = ''
    addName.value = ''
    await load()
  })
}

function refresh(id?: string) {
  return run(id ? 'refresh:' + id : 'refresh-all', async () => {
    summaries.value = []
    const resp = await api.refreshSubscription(id)
    summaries.value = (resp.data.results ?? []).map((r) => r.summary)
    await load()
  })
}

function startRename(sub: Subscription) {
  renaming.value = sub.id
  renameText.value = sub.name
}

function saveRename(sub: Subscription) {
  // Enter reaches here while the ✓ button is disabled.
  if (busy.value) return
  return run('rename:' + sub.id, async () => {
    await api.renameSubscription(sub.id, renameText.value.trim())
    renaming.value = ''
    await load()
  })
}

function remove(sub: Subscription) {
  const warn = runsFrom(sub.id)
    ? '\n\nThe running Xray server comes from it. Xray keeps running it until you select another.'
    : ''
  if (!confirm(`Delete the subscription ${sub.name} and its ${sub.servers} servers?${warn}`)) return
  return run('delete:' + sub.id, async () => {
    const resp = await api.deleteSubscription(sub.id)
    if (resp.data.active_removed) {
      alert('The running server is no longer in any subscription. Select another server.')
    }
    await load()
  })
}

async function selectServer(group: SubscriptionServers, index: number) {
  const server = group.servers?.[index]
  if (!server) return
  busy.value = `select:${group.id}:${index}`
  try {
    await api.selectServer(group.id, index, server)
    alert(`Server selected: ${group.name} / ${server.name}`)
    await load()
  } catch (e: any) {
    if (e.response?.status === 409) {
      // A refresh on the router - the bot's subscription watch, an import in
      // another tab - moved the list since it was shown.
      alert('The server list changed; it has been reloaded. Select the server again.')
      await load()
    } else {
      alert('Error: ' + errorText(e))
    }
  } finally {
    busy.value = ''
  }
}

onMounted(load)
</script>

<template>
  <div class="card">
    <div class="card-title">Subscriptions</div>

    <table v-if="subscriptions.length > 0">
      <thead>
        <tr>
          <th>Name</th>
          <th>Host</th>
          <th>Servers</th>
          <th>Refreshed</th>
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
    <details
      v-for="group in groups"
      :key="group.id"
      :open="runsFrom(group.id) || groups.length === 1"
      style="margin-bottom: 0.75rem;"
    >
      <summary style="cursor: pointer; font-weight: 600;">
        {{ group.name }} — {{ (group.servers ?? []).length }} servers
        <span v-if="runsFrom(group.id)" class="badge badge-green" style="margin-left: 0.5rem;">running</span>
      </summary>
      <table>
        <thead>
          <tr>
            <th>#</th>
            <th>Name</th>
            <th>Address</th>
            <th>Port</th>
            <th>Protocol</th>
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
            <td>
              <button class="btn btn-green" :disabled="!!busy" @click="selectServer(group, idx)">
                {{ busy === `select:${group.id}:${idx}` ? '...' : 'Select' }}
              </button>
            </td>
          </tr>
        </tbody>
      </table>
    </details>
    <p v-if="groups.length === 0 && !loading && !error" style="color: #999; font-size: 0.875rem;">
      No servers found. Add a subscription to get started.
    </p>
  </div>
</template>
