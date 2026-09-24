import axios from 'axios'
import type {
  AllLogsResponse,
  ClientsResponse,
  ConfigResponse,
  DeleteSubscriptionResponse,
  ExcludeIPsResponse,
  ExcludeSetsResponse,
  IPResponse,
  LogResponse,
  OkResponse,
  PlatformInfo,
  RefreshResponse,
  Server,
  ServersResponse,
  StatusResponse,
  SubscriptionResult,
  SubscriptionsResponse,
  UpdateCheckResponse,
  UpdateStartResponse,
  UpdateStatusResponse,
  VersionResponse,
} from './types'

const api = axios.create({
  withCredentials: true,
})

api.interceptors.response.use(
  (response) => response,
  (error) => {
    // Only reload on 401 for requests that expect auth (not login or auth-check).
    // skipAuthRedirect can be set on individual requests to suppress the reload.
    if (
      error.response?.status === 401 &&
      !error.config?.url?.includes('/api/login') &&
      !error.config?.skipAuthRedirect
    ) {
      window.location.reload()
    }
    return Promise.reject(error)
  },
)

export default {
  // Auth
  checkAuth: () =>
    api.get<VersionResponse>('/api/version', { skipAuthRedirect: true } as any),
  login: (username: string, password: string) =>
    api.post<OkResponse>('/api/login', { username, password }),
  logout: () =>
    api.post<OkResponse>('/api/logout'),

  // Status & Control
  getStatus: () =>
    api.get<StatusResponse>('/api/status'),
  apply: () =>
    api.post<OkResponse>('/api/apply'),
  restart: () =>
    api.post<OkResponse>('/api/restart'),
  stop: () =>
    api.post<OkResponse>('/api/stop'),
  updateIPsets: () =>
    api.post<OkResponse>('/api/ipsets/update'),

  // Info
  getIP: () =>
    api.get<IPResponse>('/api/ip'),
  getVersion: () =>
    api.get<VersionResponse>('/api/version'),
  getPlatform: () =>
    api.get<PlatformInfo>('/api/platform'),

  // Servers and subscriptions
  getServers: () =>
    api.get<ServersResponse>('/api/servers'),
  // The server as the page shows it at that index of that subscription: the
  // list can change before the click, and the router answers 409 when the
  // index names another server or the subscription is gone.
  selectServer: (subscription: string, index: number, server: Server) =>
    api.post<OkResponse>('/api/servers/active', {
      subscription,
      index,
      name: server.name,
      address: server.address,
      port: server.port,
    }),
  getSubscriptions: () =>
    api.get<SubscriptionsResponse>('/api/subscriptions'),
  addSubscription: (url: string, name: string) =>
    api.post<SubscriptionResult>('/api/subscriptions', { url, name }),
  // No id refreshes every subscription that has a link.
  refreshSubscription: (id?: string) =>
    api.post<RefreshResponse>('/api/subscriptions/refresh', null, { params: id ? { id } : {} }),
  renameSubscription: (id: string, name: string) =>
    api.post<OkResponse>('/api/subscriptions/rename', { name }, { params: { id } }),
  deleteSubscription: (id: string) =>
    api.delete<DeleteSubscriptionResponse>('/api/subscriptions', { params: { id } }),

  // Clients
  getClients: () =>
    api.get<ClientsResponse>('/api/clients'),
  addClient: (ip: string, route: string) =>
    api.post<OkResponse>('/api/clients', { ip, route }),
  pauseClient: (ip: string) =>
    api.post<OkResponse>('/api/clients/pause', null, { params: { ip } }),
  resumeClient: (ip: string) =>
    api.post<OkResponse>('/api/clients/resume', null, { params: { ip } }),
  deleteClient: (ip: string) =>
    api.delete<OkResponse>('/api/clients', { params: { ip } }),

  // Exclusions
  getExcludeSets: () =>
    api.get<ExcludeSetsResponse>('/api/excludes/sets'),
  updateExcludeSets: (sets: string[]) =>
    api.post<OkResponse>('/api/excludes/sets', { sets }),
  getExcludeIPs: () =>
    api.get<ExcludeIPsResponse>('/api/excludes/ips'),
  addExcludeIP: (ip: string) =>
    api.post<OkResponse>('/api/excludes/ips', { ip }),
  deleteExcludeIP: (ip: string) =>
    api.delete<OkResponse>('/api/excludes/ips', { params: { ip } }),

  // Logs & Config
  // /api/logs answers with two different shapes, so it gets two methods: one
  // file with its name, or every configured source at once.
  getLog: (source: string, lines?: number) =>
    api.get<LogResponse>('/api/logs', { params: { source, ...(lines ? { lines } : {}) } }),
  getAllLogs: (lines?: number) =>
    api.get<AllLogsResponse>('/api/logs', { params: { ...(lines ? { lines } : {}) } }),
  getConfig: () =>
    api.get<ConfigResponse>('/api/config'),

  // Self-update
  checkUpdate: (force = false) =>
    api.get<UpdateCheckResponse>('/api/update/check', { params: force ? { force: 1 } : {} }),
  update: () =>
    api.post<UpdateStartResponse>('/api/update'),
  // Bounded like pollVersion: waitForVersion awaits this inside its loop, so a
  // hung connection to the router would otherwise stop the loop from ever
  // reaching its own twenty-minute limit.
  updateStatus: () =>
    api.get<UpdateStatusResponse>('/api/update/status', { timeout: 8000 }),
  // pollVersion is used while the server restarts: connection errors and a
  // brief 401 must not bounce the user to the login page.
  pollVersion: () =>
    api.get<VersionResponse>('/api/version', {
      skipAuthRedirect: true,
      timeout: 8000,
    } as any),
}
