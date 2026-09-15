export interface Server {
  name: string
  address: string
  port: number
  uuid: string
  ips: string[]
}

export interface ClientInfo {
  ip: string
  route: string
  paused: boolean
}

export interface StatusResponse {
  output: string
}

export interface VersionResponse {
  version: string
  commit: string
}

export interface UpdateCheckResponse {
  current?: string
  latest?: string
  update_available: boolean
  changelog?: string
  checked_at?: string
  dev?: boolean
}

export interface UpdateStartResponse {
  ok: boolean
  from?: string
  to?: string
  update_available?: boolean
}

/** Every mutation answers {"ok": true}. */
export interface OkResponse {
  ok: boolean
}

export interface ImportResponse {
  ok: boolean
  count: number
}

export interface IPResponse {
  ip: string
}

/** The server the running Xray config was generated from. Null until something
 *  selects one: config.json holds only the outbound, and a subscription puts
 *  many names behind one address:port, so it cannot be read back into a name. */
export interface ActiveServer {
  name: string
  address: string
  port: number
}

/** Go marshals a nil slice as null and none of these four fields carry
 *  omitempty, so an empty router answers with null, not []. The `?? []` guards
 *  at the call sites are load-bearing; the nullable type keeps them that way. */
export interface ServersResponse {
  servers: Server[] | null
  active: ActiveServer | null
  subscription_saved?: boolean
}

export interface ClientsResponse {
  clients: ClientInfo[] | null
}

export interface ExcludeSetsResponse {
  sets: string[] | null
}

export interface ExcludeIPsResponse {
  ips: string[] | null
}

/** GET /api/logs?source=... — one file. */
export interface LogResponse {
  output: string
  source: string
}

/** GET /api/logs — source name to contents, for every configured source. */
export type AllLogsResponse = Record<string, string>

export interface UpdateStatusResponse {
  in_progress: boolean
}

/** GET /api/config returns the whole vpn-director.json with jwt_secret blanked;
 *  the Settings tab only ever re-serialises it. */
export type ConfigResponse = Record<string, unknown>

/** One firmware VPN client tunnel as `vpn-director.sh platform` lists it;
 *  `id` is what a client's route names. */
export interface PlatformTunnel {
  id: string
  iface: string
  type: string
  connected: boolean
  description: string
}

/** GET /api/platform — the router's platform facts. Go marshals nil slices as
 *  null, hence the nullable arrays. */
export interface PlatformInfo {
  platform: string
  arch: string
  password_file: string
  lan_ifaces: string[] | null
  wan_if: string
  tunnels: PlatformTunnel[] | null
}
