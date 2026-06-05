/* data.ts — mock fleet/server data + lifecycle helpers and domain types. */

export type Role = "operator" | "owner"

export type ServerStatus =
  | "running"
  | "stopped"
  | "starting"
  | "stopping"
  | "provisioning"
  | "scheduling"
  | "unschedulable"
  | "error"
  | "draining"
  | "down"
  | "ready"

export type HostStatus = "ready" | "draining" | "down"

export interface Host {
  id: string
  hostname: string
  address: string
  zone: string
  cpus: number
  mem: number
  status: HostStatus
  agent: string
  heartbeat: number
}

export interface Owner {
  id: string
  name: string
  email: string
  initials: string
}

export interface Server {
  id: string
  name: string
  owner: string
  version: string
  desired: "running" | "stopped"
  status: ServerStatus
  hostId: string | null
  cpus: number
  mem: number
  players: number
  maxPlayers: number
  address: string | null
  port: number | null
  health: string
  statusMessage: string | null
  attempts: number
  createdDays: number
  world: number
}

export const MC_VERSIONS = ["1.20.4", "1.21.1", "1.20.1", "1.19.4", "1.21.4"]
export const ZONES = ["us-east-1a", "us-east-1b", "us-west-2a", "eu-central-1a"]

export const HOSTS: Host[] = [
  { id: "host-7f3a", hostname: "fc-node-01", address: "10.0.4.11", zone: "us-east-1a", cpus: 32, mem: 131072, status: "ready", agent: "0.9.2", heartbeat: 3 },
  { id: "host-2b9c", hostname: "fc-node-02", address: "10.0.4.12", zone: "us-east-1a", cpus: 32, mem: 131072, status: "ready", agent: "0.9.2", heartbeat: 2 },
  { id: "host-c41d", hostname: "fc-node-03", address: "10.0.6.21", zone: "us-east-1b", cpus: 48, mem: 196608, status: "ready", agent: "0.9.2", heartbeat: 5 },
  { id: "host-9e02", hostname: "fc-node-04", address: "10.1.2.31", zone: "us-west-2a", cpus: 32, mem: 131072, status: "draining", agent: "0.9.1", heartbeat: 8 },
  { id: "host-5a77", hostname: "fc-node-05", address: "10.2.1.14", zone: "eu-central-1a", cpus: 48, mem: 196608, status: "ready", agent: "0.9.2", heartbeat: 4 },
  { id: "host-0d18", hostname: "fc-node-06", address: "10.0.6.22", zone: "us-east-1b", cpus: 32, mem: 131072, status: "down", agent: "0.9.1", heartbeat: 142 },
]

export const OWNERS: Owner[] = [
  { id: "u-anya", name: "Anya Petrova", email: "anya@craftling.gg", initials: "AP" },
  { id: "u-marco", name: "Marco Reyes", email: "marco@craftling.gg", initials: "MR" },
  { id: "u-jin", name: "Jin Park", email: "jin@craftling.gg", initials: "JP" },
  { id: "u-sam", name: "Sam Okafor", email: "sam@craftling.gg", initials: "SO" },
]

let _id = 100
export const nid = (p: string) =>
  `${p}-${(_id++).toString(36)}${Math.random().toString(36).slice(2, 5)}`

export interface ServerSpec {
  id?: string
  name: string
  owner: string
  version: string
  desired: "running" | "stopped"
  status: ServerStatus
  hostId?: string | null
  cpus: number
  mem: number
  players?: number
  maxPlayers?: number
  address?: string | null
  port?: number | null
  health?: string
  statusMessage?: string | null
  attempts?: number
  createdDays?: number
  world?: number
}

export function srv(o: ServerSpec): Server {
  return {
    id: o.id || nid("gs"),
    name: o.name,
    owner: o.owner,
    version: o.version,
    desired: o.desired, // running | stopped
    status: o.status, // observed
    hostId: o.hostId || null,
    cpus: o.cpus,
    mem: o.mem, // mem MB
    players: o.players || 0,
    maxPlayers: o.maxPlayers || 20,
    address: o.address || null,
    port: o.port || null,
    health: o.health || (o.status === "running" ? "healthy" : "—"),
    statusMessage: o.statusMessage || null,
    attempts: o.attempts || 0,
    createdDays: o.createdDays != null ? o.createdDays : 12,
    world: o.world || 1024 + Math.floor(Math.random() * 6000),
  }
}

export const SERVERS: Server[] = [
  srv({ name: "hermitcraft-s10", owner: "u-anya", version: "1.20.4", desired: "running", status: "running", hostId: "host-7f3a", cpus: 4, mem: 8192, players: 14, maxPlayers: 30, address: "play.craftling.gg", port: 25571, world: 5820, createdDays: 41 }),
  srv({ name: "creative-flatlands", owner: "u-anya", version: "1.21.1", desired: "running", status: "running", hostId: "host-2b9c", cpus: 2, mem: 4096, players: 3, maxPlayers: 20, address: "10.0.4.12", port: 25565, world: 1240, createdDays: 9 }),
  srv({ name: "skyblock-prod", owner: "u-marco", version: "1.20.4", desired: "running", status: "running", hostId: "host-c41d", cpus: 8, mem: 16384, players: 47, maxPlayers: 80, address: "sky.craftling.gg", port: 25565, world: 9120, createdDays: 120, health: "healthy" }),
  srv({ name: "vanilla-smp", owner: "u-jin", version: "1.21.4", desired: "running", status: "starting", hostId: "host-5a77", cpus: 4, mem: 8192, players: 0, maxPlayers: 40, address: null, port: null, world: 3400, createdDays: 2, statusMessage: "Booting microVM · pulling world archive" }),
  srv({ name: "modded-atm9", owner: "u-marco", version: "1.20.1", desired: "running", status: "error", hostId: "host-9e02", cpus: 8, mem: 24576, players: 0, maxPlayers: 24, address: null, port: null, world: 14200, createdDays: 33, attempts: 3, statusMessage: "Boot failed: rootfs checksum mismatch · retry in 28s" }),
  srv({ name: "build-sandbox", owner: "u-jin", version: "1.19.4", desired: "stopped", status: "stopped", hostId: null, cpus: 2, mem: 4096, players: 0, maxPlayers: 10, address: null, port: null, world: 760, createdDays: 64 }),
  srv({ name: "minigames-lobby", owner: "u-sam", version: "1.21.1", desired: "running", status: "running", hostId: "host-7f3a", cpus: 6, mem: 12288, players: 22, maxPlayers: 60, address: "mini.craftling.gg", port: 25580, world: 2210, createdDays: 18 }),
  srv({ name: "survival-hardcore", owner: "u-sam", version: "1.20.4", desired: "running", status: "unschedulable", hostId: null, cpus: 16, mem: 65536, players: 0, maxPlayers: 12, address: null, port: null, world: 540, createdDays: 1, statusMessage: "No ready host fits 16 vCPU / 64 GB — largest host has 48 vCPU but is at capacity" }),
  srv({ name: "pixelmon-reforged", owner: "u-anya", version: "1.20.1", desired: "running", status: "running", hostId: "host-c41d", cpus: 6, mem: 12288, players: 8, maxPlayers: 50, address: "pixel.craftling.gg", port: 25566, world: 7600, createdDays: 76 }),
  srv({ name: "redstone-lab", owner: "u-jin", version: "1.21.1", desired: "stopped", status: "stopped", hostId: null, cpus: 2, mem: 4096, players: 0, maxPlayers: 8, address: null, port: null, world: 990, createdDays: 22 }),
  srv({ name: "anarchy-2k", owner: "u-marco", version: "1.20.4", desired: "running", status: "running", hostId: "host-5a77", cpus: 8, mem: 16384, players: 61, maxPlayers: 100, address: "2k.craftling.gg", port: 25565, world: 22400, createdDays: 210 }),
  srv({ name: "test-rc-1214", owner: "u-sam", version: "1.21.4", desired: "running", status: "scheduling", hostId: null, cpus: 4, mem: 8192, players: 0, maxPlayers: 20, address: null, port: null, world: 0, createdDays: 0, statusMessage: "Selecting host with capacity…" }),
]

export const fmtMem = (mb: number) =>
  mb >= 1024
    ? (mb / 1024) % 1 === 0
      ? `${mb / 1024} GB`
      : `${(mb / 1024).toFixed(1)} GB`
    : `${mb} MB`
export const fmtWorld = (mb: number) =>
  mb >= 1024 ? `${(mb / 1024).toFixed(1)} GB` : `${mb} MB`
export const hostById = (id: string | null) => HOSTS.find((h) => h.id === id)
export const ownerById = (id: string) => OWNERS.find((o) => o.id === id)
export const ago = (s: number) =>
  s < 60 ? `${s}s ago` : s < 3600 ? `${Math.floor(s / 60)}m ago` : `${Math.floor(s / 3600)}h ago`

// transition targets when starting/stopping
export const STATUS_FLOW = {
  start: ["scheduling", "provisioning", "starting", "running"],
  stop: ["stopping", "stopped"],
}
