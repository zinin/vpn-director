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
// form's select and every row's Route select. POST /api/clients and
// /api/clients/route take xray, a tunnel the config already has, or one the
// platform lists (checkClientRoute); the routes in use are offered for all
// rows only when the platform itself could not be asked - the fallback the
// form's own message describes. A row always offers its own route as well
// (rowRouteOptions), so nothing is hidden. Rebuilt whenever the platform or
// the clients are reloaded.
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

// Only a tunnel can be down. xray is none, and neither is main, which the
// platform never lists (vpn-director.sh platform leaves it out): no question
// follows a pick of either, so the platform is not asked for one.
function canBeDown(route: string): boolean {
  return route !== 'xray' && route !== 'main'
}

// A legacy entry that is no IPv4 address (an IPv6 one an older build saved):
// no route can take it, and the router refuses to move it.
function isMovable(client: ClientInfo): boolean {
  return !client.ip.includes(':')
}

function confirmDown(route: string, verb: string): boolean {
  return confirm(
    `${route} is down: until it is up, this client's traffic goes out through the WAN. ${verb} anyway?`,
  )
}

// A tunnel the platform does not list - a typo, a connection deleted on the
// router - is one Tunnel Director skips until it is listed, and a client put
// on it goes out through the WAN meanwhile. Asked only when the platform
// answered: a failed read has no list to go by.
function isUnlisted(route: string): boolean {
  return platformError.value === '' && !platformTunnels.value.some((t) => t.id === route)
}

function confirmUnlisted(route: string, verb: string): boolean {
  return confirm(
    `${route} is not on the router's tunnel list: until it is, Tunnel Director does not route this client through it. ${verb} anyway?`,
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

// The platform as it is now, for the question asked before a move to a tunnel
// or an add on one (canBeDown): a tab left open for hours holds a tunnel state
// that may be long gone, and that question is the one warning before a client
// goes on a tunnel that is down or not listed. A reload that fails leaves the
// state already loaded.
async function refreshPlatform() {
  try {
    const resp = await api.getPlatform()
    platformTunnels.value = resp.data.tunnels ?? []
    platformError.value = ''
    buildRouteOptions()
  } catch {
    // The state loaded before is the best there is.
  }
}

// ⟳ Refresh: the clients and the tunnels alike.
async function refreshAll() {
  await Promise.all([loadClients(), loadPlatform()])
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

// The address and the route are read before the platform is asked afresh: a
// reload that no longer lists the picked tunnel puts the form back on xray, and
// the add must not follow it there.
async function addClient() {
  const ip = newIp.value.trim()
  const route = newRoute.value
  if (!ip) return
  addLoading.value = true
  try {
    if (canBeDown(route)) {
      await refreshPlatform()
      if (isDown(route) && !confirmDown(route, 'Add')) return
      if (isUnlisted(route) && !confirmUnlisted(route, 'Add')) return
    }
    await api.addClient(ip, route)
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
// carries it. The row shows the picked route, its controls disabled, from the
// pick on: for a tunnel the platform is asked afresh before the question about
// one that is down or not listed, and a render in that wait would put the
// select back. A select whose move did not happen goes back to the route the
// client is on.
async function moveClient(client: ClientInfo, event: Event) {
  const select = event.target as HTMLSelectElement
  const route = select.value
  if (route === client.route) return
  pendingMove.value = { row: rowKey(client), route }
  actionLoading.value = 'move:' + client.ip
  try {
    if (canBeDown(route)) {
      await refreshPlatform()
      if (isDown(route) && !confirmDown(route, 'Move')) {
        select.value = client.route
        return
      }
      if (isUnlisted(route) && !confirmUnlisted(route, 'Move')) {
        select.value = client.route
        return
      }
    }
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
      <button class="btn btn-blue" :disabled="loading" @click="refreshAll">
        {{ loading ? '...' : '⟳ Refresh' }}
      </button>
    </div>

    <p v-if="error" class="error-msg">{{ error }}</p>

    <!-- The table scrolls sideways inside the card: on a phone the page itself does not. -->
    <div v-if="clients.length > 0" style="overflow-x: auto;">
      <table>
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
                v-if="isMovable(client)"
                :value="shownRoute(client)"
                :disabled="!!actionLoading"
                style="min-width: 160px;"
                @change="moveClient(client, $event)"
              >
                <option v-for="r in rowRouteOptions(client)" :key="r.value" :value="r.value">
                  {{ r.label }}
                </option>
              </select>
              <span v-else>{{ client.route }}</span>
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
    </div>

    <p v-else-if="!loading" style="color: #999; font-size: 0.875rem;">
      No clients configured.
    </p>
  </div>
</template>
