<script setup lang="ts">
import { ref, computed, onMounted, onUnmounted } from 'vue'
import api from '../api'
import { monitorText } from '../monitor'
import WatchStatus from './WatchStatus.vue'
import type { ActiveServer, MonitorResponse, ServersResponse, WatchResponse } from '../types'

const status = ref('')
const ip = ref('')
const ipError = ref('')
const activeServer = ref<ActiveServer | null>(null)
const activeLabel = ref('')
const serverError = ref('')
// "Nothing is selected" and "we have not asked yet" look identical in
// activeServer, and only the first of them is worth telling the user about.
const serverLoaded = ref(false)
const loading = ref(false)
const actionLoading = ref('')
const monitor = ref<MonitorResponse | null>(null)
const monitorUnavailable = ref(false)
const watch = ref<WatchResponse | null>(null)
const watchUnavailable = ref(false)
let unmounted = false
let statusRequest = 0
let monitorRequest = 0
let watchRequest = 0

function errorText(e: any): string {
  return e?.response?.data?.error || e?.message || 'unknown error'
}

// "Beta / Germany-1": the running server under its subscription's name, or
// its own name when no subscription holds it any more - deleted since, or a
// record from before subscriptions.
function serverLabel(data: ServersResponse): string {
  const a = data.active
  if (!a) return ''
  const name = a.name || a.address
  const group = (data.subscriptions ?? []).find((g) => g.id === a.subscription)
  return group ? `${group.name} / ${name}` : `${name} — not in any subscription`
}

async function loadOutput(request: number) {
  if (unmounted || request !== statusRequest) return
  try {
    const resp = await api.getStatus()
    if (unmounted || request !== statusRequest) return
    status.value = resp.data.output
  } catch (e: any) {
    if (unmounted || request !== statusRequest) return
    status.value = 'Error: ' + errorText(e)
  }
}

async function loadIP(request: number) {
  if (unmounted || request !== statusRequest) return
  try {
    const resp = await api.getIP()
    if (unmounted || request !== statusRequest) return
    ip.value = resp.data.ip
  } catch (e: any) {
    if (unmounted || request !== statusRequest) return
    ip.value = ''
    ipError.value = errorText(e)
  }
}

async function loadServers(request: number) {
  if (unmounted || request !== statusRequest) return
  try {
    const resp = await api.getServers()
    if (unmounted || request !== statusRequest) return
    activeServer.value = resp.data.active ?? null
    activeLabel.value = serverLabel(resp.data)
    serverLoaded.value = true
  } catch (e: any) {
    if (unmounted || request !== statusRequest) return
    activeServer.value = null
    serverError.value = errorText(e)
  }
}

async function loadMonitor() {
  if (unmounted) return
  const request = ++monitorRequest
  try {
    const resp = await api.getMonitor()
    if (unmounted || request !== monitorRequest) return
    monitor.value = resp.data
    monitorUnavailable.value = false
  } catch {
    if (unmounted || request !== monitorRequest) return
    monitor.value = null
    monitorUnavailable.value = true
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

const monitorLine = computed(() => monitorText(monitor.value, monitorUnavailable.value))

async function loadStatus() {
  if (unmounted) return
  const request = ++statusRequest
  loading.value = true
  ipError.value = ''
  serverError.value = ''
  // Each read publishes immediately; a pending IPC must not hold the other results.
  await Promise.all([
    loadOutput(request),
    loadIP(request),
    loadServers(request),
    loadMonitor(),
    loadWatch(),
  ])
  if (!unmounted && request === statusRequest) loading.value = false
}

async function doAction(name: string, fn: () => Promise<any>) {
  if (unmounted || actionLoading.value) return
  actionLoading.value = name
  try {
    await fn()
    if (unmounted) return
    await loadStatus()
  } catch (e: any) {
    if (unmounted) return
    alert('Error: ' + errorText(e))
    await loadStatus()
  } finally {
    if (!unmounted) actionLoading.value = ''
  }
}

onMounted(loadStatus)
onUnmounted(() => {
  unmounted = true
  statusRequest++
  monitorRequest++
  watchRequest++
})
</script>

<template>
  <div class="actions">
    <button class="btn btn-green" :disabled="!!actionLoading" @click="doAction('apply', api.apply)">
      {{ actionLoading === 'apply' ? '...' : '▶ Apply' }}
    </button>
    <button class="btn btn-yellow" :disabled="!!actionLoading" @click="doAction('restart', api.restart)">
      {{ actionLoading === 'restart' ? '...' : '↻ Restart' }}
    </button>
    <button class="btn btn-red" :disabled="!!actionLoading" @click="doAction('stop', api.stop)">
      {{ actionLoading === 'stop' ? '...' : '■ Stop' }}
    </button>
    <button class="btn btn-blue" :disabled="!!actionLoading" @click="doAction('ipsets', api.updateIPsets)">
      {{ actionLoading === 'ipsets' ? '...' : '⟳ Update IPsets' }}
    </button>
    <button class="btn btn-blue" :disabled="loading" @click="loadStatus">
      {{ loading ? '...' : '⟳ Refresh' }}
    </button>
  </div>

  <div class="card">
    <div class="card-title">Monitoring and automation</div>
    <p v-if="monitorLine" style="font-size: 0.875rem; margin-bottom: 0.75rem; overflow-wrap: anywhere;">{{ monitorLine }}</p>
    <WatchStatus :snapshot="watch" :unavailable="watchUnavailable" />
  </div>

  <div class="grid-2">
    <div class="card">
      <div class="card-title">Status</div>
      <pre style="font-size: 12px; white-space: pre-wrap; line-height: 1.6;">{{ status || 'Loading...' }}</pre>
    </div>
    <div>
      <div class="card">
        <div class="card-title">Xray Server</div>
        <div v-if="activeServer">
          <div style="font-size: 20px; margin-top: 8px;">
            {{ activeLabel }}
          </div>
          <div style="margin-top: 4px; color: #999; font-size: 0.875rem;">
            {{ activeServer.address }}:{{ activeServer.port }}
          </div>
        </div>
        <div v-else-if="serverError" style="margin-top: 8px; color: #ff6b6b; font-size: 0.875rem;">
          unavailable: {{ serverError }}
        </div>
        <div v-else-if="serverLoaded" style="margin-top: 8px; color: #999; font-size: 0.875rem;">
          none selected yet — pick one on the Servers tab
        </div>
        <div v-else style="font-size: 20px; margin-top: 8px;">...</div>
      </div>

      <div class="card">
        <div class="card-title">External IP</div>
        <div v-if="ip" style="font-size: 20px; margin-top: 8px;">{{ ip }}</div>
        <div v-else-if="ipError" style="margin-top: 8px; color: #ff6b6b; font-size: 0.875rem;">
          unavailable: {{ ipError }}
        </div>
        <div v-else style="font-size: 20px; margin-top: 8px;">...</div>
      </div>
    </div>
  </div>
</template>
