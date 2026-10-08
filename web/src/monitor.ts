import type { MonitorResponse } from './types'

export function monitorText(m: MonitorResponse | null, unavailable: boolean): string {
  if (unavailable) return 'Monitoring: unavailable'
  if (!m) return ''
  switch (m.state) {
    case 'ok': {
      if (m.lag_seconds > m.interval_seconds) return 'Monitoring: checks are falling behind'
      const s = m.interval_seconds
      return `Monitoring every ${s % 60 === 0 ? `${s / 60} min` : `${s} s`}`
    }
    case 'stopped':
      return 'Monitoring: stopped with VPN Director'
    case 'disabled':
      return 'Monitoring: disabled in settings'
    case 'no_xray':
      return 'Monitoring: xray not found'
    case 'wan_down':
      return 'Monitoring: WAN down, statuses kept'
    case 'prober_error':
      return `Monitoring: the prober does not start: ${m.message ?? ''}`
    default:
      return 'Monitoring: not running'
  }
}
