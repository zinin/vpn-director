<script setup lang="ts">
import { computed } from 'vue'
import { watchText } from '../watch'
import type { WatchResponse } from '../types'

const props = defineProps<{ snapshot: WatchResponse | null; unavailable: boolean }>()

function detail(value?: string): string {
  const characters = Array.from(value ?? '')
  return characters.length > 512 ? characters.slice(0, 512).join('') + '…' : characters.join('')
}

const message = computed(() => detail(props.snapshot?.message))
const storageError = computed(() => detail(props.snapshot?.notifications.storage_error))
const action = computed(() => {
  switch (props.snapshot?.action) {
    case 'checking':
      return 'Checking the active server'
    case 'switching':
      return 'Switching to another server'
    case 'fallback':
      return 'Fallback routing'
    case 'refreshing':
      return 'Refreshing subscriptions'
    case 'walking':
      return 'Walking subscription servers'
    case 'returning':
      return 'Returning to the preferred server'
    case 'restoring':
      return 'Restoring client routes'
    default:
      return ''
  }
})
</script>

<template>
  <div class="watch-status" role="status" aria-live="polite">
    <p>{{ watchText(props.snapshot, props.unavailable) }}</p>
    <template v-if="props.snapshot && !props.unavailable">
      <p v-if="message" class="kv-label">{{ message }}</p>
      <p v-if="action" class="kv-label">{{ action }}</p>
      <p v-if="props.snapshot.committed_failover" class="kv-label">Committed failover: fallback routes are in use.</p>
      <p v-if="props.snapshot.pending_restore" class="kv-label">Pending restore: client routes still need to be restored.</p>
      <p class="kv-label">Notifications: {{ props.snapshot.notifications.pending }} pending</p>
      <p v-if="storageError" class="error-msg">{{ storageError }}</p>
    </template>
  </div>
</template>

<style scoped>
.watch-status {
  display: flex;
  flex-direction: column;
  gap: 0.35rem;
  min-width: 0;
  max-width: 100%;
  font-size: 0.875rem;
  line-height: 1.5;
  overflow-wrap: anywhere;
  white-space: pre-wrap;
}
</style>
