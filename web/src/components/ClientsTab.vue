<script setup lang="ts">
import { ref, onMounted } from 'vue'
import api from '../api'
import type { ClientInfo, PlatformTunnel } from '../types'

const clients = ref<ClientInfo[]>([])
const loading = ref(false)
const actionLoading = ref('')
const error = ref('')

const newIp = ref('')
const newRoute = ref('xray')
const addLoading = ref(false)

interface RouteOption {
  value: string
  label: string
}

// xray first, then every tunnel the router has. These options feed the Add
// form's select and every row's Route select, and POST /api/clients and
// /api/clients/route reject a route the platform does not list, so a listed
// client's unlisted route is offered only when the platform itself could not
// be asked - the fallback the form's own message describes. A row always
// offers its own route as well (rowRouteOptions), so nothing is hidden.
// Rebuilt whenever the platform or the clients are reloaded.
const routeOptions = ref<RouteOption[]>([{ value: 'xray', label: 'xray' }])
const platformTunnels = ref<PlatformTunnel[]>([])
const platformError = ref('')

function buildRouteOptions() {
  const opts: RouteOption[] = [{ value: 'xray', label: 'xray' }]
  for (const t of platformTunnels.value) {
    const desc = t.description ? ` — ${t.description}` : ''
    opts.push({ value: t.id, label: `${t.id}${desc}${t.connected ? '' : ' (down)'}` })
  }
  if (platformError.value !== '') {
    for (const c of clients.value) {
      if (!opts.some((o) => o.value === c.route)) {
        opts.push({ value: c.route, label: c.route })
      }
    }
  }
  routeOptions.value = opts
  if (!opts.some((o) => o.value === newRoute.value)) {
    newRoute.value = 'xray'
  }
}

// A row's options are the shared ones plus the row's own route, so its select
// never shows blank for a tunnel the platform no longer lists.
function rowRouteOptions(client: ClientInfo): RouteOption[] {
  if (routeOptions.value.some((o) => o.value === client.route)) {
    return routeOptions.value
  }
  return [...routeOptions.value, { value: client.route, label: client.route }]
}

// A tunnel the platform reports down has no route in its table, and a client
// put on it goes out through the WAN until it comes up.
function isDown(route: string): boolean {
  return platformTunnels.value.some((t) => t.id === route && !t.connected)
}

function confirmDown(route: string, verb: string): boolean {
  return confirm(
    `${route} is down: until it is up, this client's traffic goes out through the WAN. ${verb} anyway?`,
  )
}

async function loadPlatform() {
  try {
    const resp = await api.getPlatform()
    platformTunnels.value = resp.data.tunnels ?? []
    platformError.value = ''
  } catch (e: any) {
    platformTunnels.value = []
    platformError.value = e.response?.data?.error || e.message
  }
  buildRouteOptions()
}

// Shows the server error; when the change was saved but apply failed
// (response carries saved: true) the list is refreshed so the saved
// change is visible and the Status tab's Apply can retry. A 404 is
// refreshed too: the server says the row does not exist, so the displayed
// list is stale and is exactly what must be reloaded.
async function reportError(e: any) {
  alert('Error: ' + (e.response?.data?.error || e.message))
  if (e.response?.data?.saved || e.response?.status === 404) {
    await loadClients()
  }
}

async function loadClients() {
  loading.value = true
  error.value = ''
  try {
    const resp = await api.getClients()
    clients.value = resp.data.clients ?? []
    buildRouteOptions()
  } catch (e: any) {
    error.value = e.response?.data?.error || e.message
  } finally {
    loading.value = false
  }
}

async function addClient() {
  if (!newIp.value.trim()) return
  if (isDown(newRoute.value) && !confirmDown(newRoute.value, 'Add')) return
  addLoading.value = true
  try {
    await api.addClient(newIp.value.trim(), newRoute.value)
    newIp.value = ''
    newRoute.value = 'xray'
    await loadClients()
  } catch (e: any) {
    await reportError(e)
  } finally {
    addLoading.value = false
  }
}

// The row whose move is running, keyed ip|route like the rows, and the route
// picked in it. Vue sets a select's value on every render, so the render that
// disables the controls for the move would put that select back on the old
// route for as long as the apply takes; the row shows the picked route instead
// until the request ends.
const pendingMove = ref<{ row: string; route: string } | null>(null)

// ip|route: the key of a client's row, in the table and in pendingMove.
function rowKey(client: ClientInfo): string {
  return client.ip + '|' + client.route
}

// The route a row's select shows: the one picked in it while its move runs,
// the route the client is on otherwise.
function shownRoute(client: ClientInfo): string {
  const m = pendingMove.value
  return m !== null && m.row === rowKey(client) ? m.route : client.route
}

// One request moves the client: the router writes the new route and applies
// once, and the apply keeps the client on its old route until the new one
// carries it. A select whose move did not happen goes back to the route the
// client is on.
async function moveClient(client: ClientInfo, event: Event) {
  const select = event.target as HTMLSelectElement
  const route = select.value
  if (route === client.route) return
  if (isDown(route) && !confirmDown(route, 'Move')) {
    select.value = client.route
    return
  }
  pendingMove.value = { row: rowKey(client), route }
  actionLoading.value = 'move:' + client.ip
  try {
    await api.moveClient(client.ip, route)
    await loadClients()
  } catch (e: any) {
    pendingMove.value = null
    select.value = client.route
    await reportError(e)
  } finally {
    pendingMove.value = null
    actionLoading.value = ''
  }
}

async function pauseClient(ip: string) {
  actionLoading.value = 'pause:' + ip
  try {
    await api.pauseClient(ip)
    await loadClients()
  } catch (e: any) {
    await reportError(e)
  } finally {
    actionLoading.value = ''
  }
}

async function resumeClient(ip: string) {
  actionLoading.value = 'resume:' + ip
  try {
    await api.resumeClient(ip)
    await loadClients()
  } catch (e: any) {
    await reportError(e)
  } finally {
    actionLoading.value = ''
  }
}

async function removeClient(ip: string) {
  if (!confirm('Remove client ' + ip + '?')) return
  actionLoading.value = 'remove:' + ip
  try {
    await api.deleteClient(ip)
    await loadClients()
  } catch (e: any) {
    await reportError(e)
  } finally {
    actionLoading.value = ''
  }
}

onMounted(async () => {
  await Promise.all([loadClients(), loadPlatform()])
})
</script>

<template>
  <div class="card">
    <div class="card-title">Add Client</div>
    <div style="display: flex; gap: 0.5rem; align-items: flex-end; flex-wrap: wrap;">
      <div class="form-group" style="flex: 1; min-width: 180px; margin-bottom: 0;">
        <label>IP / CIDR</label>
        <input
          v-model="newIp"
          placeholder="192.168.50.10 or 192.168.50.0/24"
          @keyup.enter="addClient"
        />
      </div>
      <div class="form-group" style="width: 260px; margin-bottom: 0;">
        <label>Route</label>
        <select v-model="newRoute">
          <option v-for="r in routeOptions" :key="r.value" :value="r.value">{{ r.label }}</option>
        </select>
      </div>
      <button class="btn btn-primary" :disabled="addLoading || !newIp.trim()" @click="addClient">
        {{ addLoading ? '...' : '+ Add' }}
      </button>
    </div>
    <p v-if="platformError" class="error-msg" style="margin-top: 0.5rem;">
      Tunnel list unavailable ({{ platformError }}); only xray and the routes in use are offered.
    </p>
  </div>

  <div class="card">
    <div class="card-title">Clients</div>

    <div class="actions">
      <button class="btn btn-blue" :disabled="loading" @click="loadClients">
        {{ loading ? '...' : '⟳ Refresh' }}
      </button>
    </div>

    <p v-if="error" class="error-msg">{{ error }}</p>

    <table v-if="clients.length > 0">
      <thead>
        <tr>
          <th>IP</th>
          <th>Route</th>
          <th>Status</th>
          <th>Actions</th>
        </tr>
      </thead>
      <tbody>
        <!-- ip|route: during a staged failover one address sits on two routes. -->
        <tr v-for="client in clients" :key="rowKey(client)">
          <td>{{ client.ip }}</td>
          <td>
            <select
              :value="shownRoute(client)"
              :disabled="!!actionLoading"
              style="min-width: 160px;"
              @change="moveClient(client, $event)"
            >
              <option v-for="r in rowRouteOptions(client)" :key="r.value" :value="r.value">
                {{ r.label }}
              </option>
            </select>
          </td>
          <td>
            <span v-if="!client.paused" class="badge badge-green">Active</span>
            <span v-else class="badge badge-grey">Paused</span>
          </td>
          <td style="display: flex; gap: 0.35rem;">
            <button
              v-if="!client.paused"
              class="btn btn-yellow"
              :disabled="!!actionLoading"
              @click="pauseClient(client.ip)"
            >
              {{ actionLoading === 'pause:' + client.ip ? '...' : 'Pause' }}
            </button>
            <button
              v-else
              class="btn btn-green"
              :disabled="!!actionLoading"
              @click="resumeClient(client.ip)"
            >
              {{ actionLoading === 'resume:' + client.ip ? '...' : 'Resume' }}
            </button>
            <button
              class="btn btn-red"
              :disabled="!!actionLoading"
              @click="removeClient(client.ip)"
            >
              {{ actionLoading === 'remove:' + client.ip ? '...' : 'Remove' }}
            </button>
          </td>
        </tr>
      </tbody>
    </table>

    <p v-else-if="!loading" style="color: #999; font-size: 0.875rem;">
      No clients configured.
    </p>
  </div>
</template>
