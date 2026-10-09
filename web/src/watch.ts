import type { WatchResponse } from './types'

export function watchText(w: WatchResponse | null, unavailable: boolean): string {
  if (unavailable) return 'Automation: unavailable'
  if (!w) return 'Automation: loading...'
  switch (w.state) {
    case 'starting':
      return 'Automation: starting'
    case 'active':
      return 'Automation: active'
    case 'stopped':
      return 'Automation: stopped'
    case 'incompatible':
      return 'Automation: incompatible — waiting for a compatible bot'
    case 'error':
      return 'Automation: error'
    case 'not_running':
      return 'Automation: not running'
    default:
      return 'Automation: unavailable'
  }
}
