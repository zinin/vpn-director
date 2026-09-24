<script setup lang="ts">
import { ref, onMounted } from 'vue'
import api from '../api'
import type { ActiveServer, ServersResponse } from '../types'

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

async function loadStatus() {
  loading.value = true
  ipError.value = ''
  serverError.value = ''
  // The three load independently: a failing curl ifconfig.me must not hide the
  // VPN Director status, and neither must an unreadable server list.
  const [statusRes, ipRes, serversRes] = await Promise.allSettled([
    api.getStatus(),
    api.getIP(),
    api.getServers(),
  ])
  if (statusRes.status === 'fulfilled') {
    status.value = statusRes.value.data.output
  } else {
    status.value = 'Error: ' + errorText(statusRes.reason)
  }
  if (ipRes.status === 'fulfilled') {
    ip.value = ipRes.value.data.ip
  } else {
    ip.value = ''
    ipError.value = errorText(ipRes.reason)
  }
  if (serversRes.status === 'fulfilled') {
    activeServer.value = serversRes.value.data.active ?? null
    activeLabel.value = serverLabel(serversRes.value.data)
    serverLoaded.value = true
  } else {
    activeServer.value = null
    serverError.value = errorText(serversRes.reason)
  }
  loading.value = false
}

async function doAction(name: string, fn: () => Promise<any>) {
  actionLoading.value = name
  try {
    await fn()
    await loadStatus()
  } catch (e: any) {
    alert('Error: ' + errorText(e))
  } finally {
    actionLoading.value = ''
  }
}

onMounted(loadStatus)
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
