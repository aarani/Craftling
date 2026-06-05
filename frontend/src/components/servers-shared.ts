/* servers-shared.ts — sizes, filters, fleet caps shared by the servers view + drawers. */
import type { Server } from "@/lib/data"

export interface SizePreset {
  id: string
  label: string
  cpus: number
  mem: number
  hint: string
}

export const SIZES: SizePreset[] = [
  { id: "small", label: "Small", cpus: 2, mem: 4096, hint: "build / testing" },
  { id: "medium", label: "Medium", cpus: 4, mem: 8192, hint: "small SMP" },
  { id: "large", label: "Large", cpus: 8, mem: 16384, hint: "busy / modded" },
  { id: "xlarge", label: "X-Large", cpus: 16, mem: 65536, hint: "big network" },
  { id: "huge", label: "Huge", cpus: 64, mem: 262144, hint: "exceeds fleet" },
]

export const MAX_HOST_CPU = 48
export const MAX_HOST_MEM = 196608

export interface FilterDef {
  id: string
  label: string
  match?: (s: Server) => boolean
}

export const FILTERS: FilterDef[] = [
  { id: "all", label: "All" },
  { id: "running", label: "Running", match: (s) => s.status === "running" },
  {
    id: "active",
    label: "Transitioning",
    match: (s) => ["scheduling", "provisioning", "starting", "stopping"].includes(s.status),
  },
  { id: "stopped", label: "Stopped", match: (s) => s.status === "stopped" },
  { id: "issues", label: "Issues", match: (s) => ["error", "unschedulable", "down"].includes(s.status) },
]
