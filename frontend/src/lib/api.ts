/* api.ts — typed client for the craftling-go control-plane API.
 *
 * Talks to /api/v1 (proxied to the Go server in dev, same-origin in prod).
 * Handles bearer-token auth with transparent refresh-token rotation, and
 * adapts the backend's GameServer shape into the UI's richer Server model. */
import type { Server, ServerStatus } from "./data"

const BASE = "/api/v1"

const ACCESS_KEY = "cl-access"
const REFRESH_KEY = "cl-refresh"

// ── Wire types (match the Go JSON exactly) ──────────────────────────────────

export interface TokenResponse {
  access_token: string
  refresh_token: string
  token_type: string
  expires_in: number
}

export type ApiRole = "user" | "admin"

export interface ApiUser {
  id: string
  email: string
  role: ApiRole
  created_at: string
}

export type ApiDesiredState = "running" | "stopped" | "deleted"

export type ApiStatus =
  | "pending"
  | "provisioning"
  | "running"
  | "stopping"
  | "stopped"
  | "deleting"
  | "deleted"
  | "error"

export interface ApiServer {
  id: string
  owner_id: string
  name: string
  game: string
  version: string
  cpus: number
  memory_mb: number
  desired_state: ApiDesiredState
  status: ApiStatus
  vm_id?: string | null
  host?: string | null
  port?: number | null
  status_message?: string | null
  created_at: string
  updated_at: string
}

export interface CreateServerInput {
  name: string
  version: string
  cpus?: number
  memory_mb?: number
}

export interface UpdateServerInput {
  name?: string
  version?: string
  desired_state?: "running" | "stopped"
}

// ── Errors ──────────────────────────────────────────────────────────────────

export class ApiError extends Error {
  status: number
  constructor(status: number, message: string) {
    super(message)
    this.name = "ApiError"
    this.status = status
  }
}

// ── Token storage ─────────────────────────────────────────────────────────--

export const tokenStore = {
  get access() {
    return localStorage.getItem(ACCESS_KEY)
  },
  get refresh() {
    return localStorage.getItem(REFRESH_KEY)
  },
  save(t: TokenResponse) {
    localStorage.setItem(ACCESS_KEY, t.access_token)
    localStorage.setItem(REFRESH_KEY, t.refresh_token)
  },
  clear() {
    localStorage.removeItem(ACCESS_KEY)
    localStorage.removeItem(REFRESH_KEY)
  },
}

// ── Transport ─────────────────────────────────────────────────────────────--

function send(path: string, init: RequestInit, withAuth: boolean): Promise<Response> {
  const headers = new Headers(init.headers)
  if (init.body) headers.set("Content-Type", "application/json")
  if (withAuth && tokenStore.access) {
    headers.set("Authorization", `Bearer ${tokenStore.access}`)
  }
  return fetch(BASE + path, { ...init, headers })
}

// In-flight refresh shared across concurrent 401s so we rotate the token once.
let refreshing: Promise<boolean> | null = null

function refreshTokens(): Promise<boolean> {
  if (!tokenStore.refresh) return Promise.resolve(false)
  if (!refreshing) {
    refreshing = (async () => {
      try {
        const res = await send(
          "/auth/refresh",
          { method: "POST", body: JSON.stringify({ refresh_token: tokenStore.refresh }) },
          false
        )
        if (!res.ok) {
          tokenStore.clear()
          return false
        }
        tokenStore.save((await res.json()) as TokenResponse)
        return true
      } catch {
        return false
      } finally {
        refreshing = null
      }
    })()
  }
  return refreshing
}

async function request<T>(
  path: string,
  init: RequestInit = {},
  opts: { auth?: boolean } = {}
): Promise<T> {
  const withAuth = opts.auth ?? true

  let res = await send(path, init, withAuth)
  if (res.status === 401 && withAuth && (await refreshTokens())) {
    res = await send(path, init, withAuth)
  }

  if (!res.ok) {
    let message = res.statusText
    try {
      const body = await res.json()
      if (body?.error) message = body.error
    } catch {
      /* non-JSON error body */
    }
    throw new ApiError(res.status, message)
  }

  if (res.status === 204) return undefined as T
  return (await res.json()) as T
}

const delay = (ms: number) => new Promise((r) => setTimeout(r, ms))

// ── API surface ─────────────────────────────────────────────────────────────

export const api = {
  async register(email: string, password: string): Promise<TokenResponse> {
    const t = await request<TokenResponse>(
      "/auth/register",
      { method: "POST", body: JSON.stringify({ email, password }) },
      { auth: false }
    )
    tokenStore.save(t)
    return t
  },

  async login(email: string, password: string): Promise<TokenResponse> {
    const t = await request<TokenResponse>(
      "/auth/login",
      { method: "POST", body: JSON.stringify({ email, password }) },
      { auth: false }
    )
    tokenStore.save(t)
    return t
  },

  async logout(): Promise<void> {
    const rt = tokenStore.refresh
    if (rt) {
      try {
        await request(
          "/auth/logout",
          { method: "POST", body: JSON.stringify({ refresh_token: rt }) },
          { auth: false }
        )
      } catch {
        /* best-effort; clear locally regardless */
      }
    }
    tokenStore.clear()
  },

  me(): Promise<ApiUser> {
    return request<ApiUser>("/me")
  },

  // Owner-scoped server list.
  listServers(): Promise<ApiServer[]> {
    return request<{ servers: ApiServer[] | null }>("/servers").then((r) => r.servers ?? [])
  },

  // Admin-only: every server across all owners.
  adminListServers(): Promise<ApiServer[]> {
    return request<{ servers: ApiServer[] | null }>("/admin/servers").then((r) => r.servers ?? [])
  },

  // Admin-only: every user.
  adminListUsers(): Promise<ApiUser[]> {
    return request<{ users: ApiUser[] | null }>("/admin/users").then((r) => r.users ?? [])
  },

  createServer(input: CreateServerInput): Promise<ApiServer> {
    return request<ApiServer>("/servers", { method: "POST", body: JSON.stringify(input) })
  },

  getServer(id: string): Promise<ApiServer> {
    return request<ApiServer>(`/servers/${id}`)
  },

  updateServer(id: string, input: UpdateServerInput): Promise<ApiServer> {
    return request<ApiServer>(`/servers/${id}`, { method: "PATCH", body: JSON.stringify(input) })
  },

  async deleteServer(id: string): Promise<void> {
    await request<unknown>(`/servers/${id}`, { method: "DELETE" })
  },

  // The control plane has no atomic restart, so drive the desired state down to
  // stopped, wait for the reconciler to converge, then back up to running.
  async restartServer(id: string): Promise<void> {
    await this.updateServer(id, { desired_state: "stopped" })
    const deadline = Date.now() + 30_000
    while (Date.now() < deadline) {
      await delay(800)
      const s = await this.getServer(id)
      if (s.status === "stopped") break
    }
    await this.updateServer(id, { desired_state: "running" })
  },
}

// ── Adapter: ApiServer → UI Server ──────────────────────────────────────────

const STATUS_MAP: Record<ApiStatus, ServerStatus> = {
  pending: "scheduling",
  provisioning: "provisioning",
  running: "running",
  stopping: "stopping",
  stopped: "stopped",
  deleting: "stopping",
  deleted: "stopped",
  error: "error",
}

function daysSince(iso: string): number {
  const t = new Date(iso).getTime()
  if (Number.isNaN(t)) return 0
  return Math.max(0, Math.floor((Date.now() - t) / 86_400_000))
}

/** Map a backend GameServer to the view model the UI renders. Fields the
 *  control plane doesn't track yet (players, world size, RCON health) are left
 *  empty and shown as "—" downstream. */
export function toServer(a: ApiServer): Server {
  return {
    id: a.id,
    name: a.name,
    owner: a.owner_id,
    version: a.version,
    desired: a.desired_state === "running" ? "running" : "stopped",
    status: STATUS_MAP[a.status] ?? "stopped",
    hostId: null,
    cpus: a.cpus,
    mem: a.memory_mb,
    players: 0,
    maxPlayers: 0,
    address: a.host ?? null,
    port: a.port ?? null,
    health: "—",
    statusMessage: a.status_message ?? null,
    attempts: 0,
    createdDays: daysSince(a.created_at),
    world: 0,
  }
}
