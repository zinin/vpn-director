<script setup lang="ts">
import { ref, onMounted } from 'vue'
import api from '../api'
import type { ActiveServer, Server } from '../types'

const servers = ref<Server[]>([])
const active = ref<ActiveServer | null>(null)
const subscriptionSaved = ref(false)
const loading = ref(false)
const importLoading = ref(false)
const selectLoading = ref(-1)
const importUrl = ref('')
const error = ref('')

async function loadServers() {
  loading.value = true
  error.value = ''
  try {
    const resp = await api.getServers()
    servers.value = resp.data.servers ?? []
    active.value = resp.data.active ?? null
    subscriptionSaved.value = !!resp.data.subscription_saved
  } catch (e: any) {
    error.value = e.response?.data?.error || e.message
  } finally {
    loading.value = false
  }
}

// Name as well as endpoint: eight of the servers in a real subscription can
// share one address:port, and matching on that alone lights up all of them.
function isActive(server: Server): boolean {
  const a = active.value
  return (
    a !== null &&
    a.name === server.name &&
    a.address === server.address &&
    a.port === server.port
  )
}

async function selectServer(index: number) {
  const server = servers.value[index]
  if (!server) return
  selectLoading.value = index
  try {
    await api.selectServer(index, server)
    alert('Server selected: ' + server.name)
    await loadServers()
  } catch (e: any) {
    if (e.response?.status === 409) {
      // A refresh on the router - the bot's subscription watch, an import in
      // another tab - moved the list since it was shown.
      alert('The server list changed; it has been reloaded. Select the server again.')
      await loadServers()
    } else {
      alert('Error: ' + (e.response?.data?.error || e.message))
    }
  } finally {
    selectLoading.value = -1
  }
}

async function importServers(url: string) {
  if (!url && !subscriptionSaved.value) {
    alert('Please enter a subscription URL')
    return
  }
  importLoading.value = true
  try {
    await api.importServers(url)
    await loadServers()
  } catch (e: any) {
    alert('Error: ' + (e.response?.data?.error || e.message))
  } finally {
    importLoading.value = false
  }
}

onMounted(loadServers)
</script>

<template>
  <div class="card">
    <div class="card-title">Servers</div>

    <div class="actions" style="display: flex; gap: 0.5rem; align-items: center; flex-wrap: wrap;">
      <button class="btn btn-blue" :disabled="loading" @click="loadServers">
        {{ loading ? '...' : '⟳ Refresh' }}
      </button>
      <input v-model="importUrl" type="text" placeholder="https://... subscription URL" style="flex: 1; min-width: 200px;" />
      <button class="btn btn-primary" :disabled="importLoading || !importUrl" @click="importServers(importUrl)">
        {{ importLoading ? '...' : '⬇ Import' }}
      </button>
      <button
        class="btn btn-blue"
        :disabled="importLoading || !subscriptionSaved"
        @click="importServers('')"
      >
        {{ importLoading ? '...' : '⬇ Re-import saved' }}
      </button>
    </div>

    <p v-if="error" class="error-msg">{{ error }}</p>

    <table v-if="servers.length > 0">
      <thead>
        <tr>
          <th>#</th>
          <th>Name</th>
          <th>Address</th>
          <th>Port</th>
          <th>Action</th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="(server, idx) in servers" :key="idx">
          <td>{{ idx + 1 }}</td>
          <td>
            {{ server.name }}
            <span v-if="isActive(server)" class="badge badge-green" style="margin-left: 0.5rem;">Active</span>
          </td>
          <td>{{ server.address }}</td>
          <td>{{ server.port }}</td>
          <td>
            <button
              class="btn btn-green"
              :disabled="selectLoading >= 0"
              @click="selectServer(idx)"
            >
              {{ selectLoading === idx ? '...' : 'Select' }}
            </button>
          </td>
        </tr>
      </tbody>
    </table>

    <p v-else-if="!loading" style="color: #999; font-size: 0.875rem;">
      No servers found. Import a subscription to get started.
    </p>
  </div>
</template>
