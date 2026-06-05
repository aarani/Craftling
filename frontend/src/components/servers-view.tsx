/* servers-view.tsx — Game Servers view: table, filters, lifecycle, detail + create drawers. */
import { useCallback, useEffect, useRef, useState } from "react"
import { Icon } from "./icon"
import { Btn, CopyBtn, Menu, Meter, StatusBadge } from "./primitives"
import { ServerDrawer, CreateDrawer, type CreateSpec } from "./drawers"
import { FILTERS, MAX_HOST_CPU, MAX_HOST_MEM } from "./servers-shared"
import {
  HOSTS,
  SERVERS,
  fmtMem,
  hostById,
  ownerById,
  srv,
  type Role,
  type Server,
  type ServerStatus,
} from "@/lib/data"

type Patch = Partial<Server>
type Step = [ServerStatus, number, ((s: Server) => Patch)?]

const TRANSITIONING: ServerStatus[] = ["scheduling", "provisioning", "starting", "stopping"]
const CAN_START: ServerStatus[] = ["stopped", "error", "unschedulable"]
const CAN_STOP: ServerStatus[] = ["running", "starting", "provisioning"]

export function ServersView({
  role,
  zone,
  onCountChange,
}: {
  role: Role
  zone: string
  onCountChange?: (n: number) => void
}) {
  const [servers, setServers] = useState<Server[]>(() => SERVERS.map((s) => ({ ...s })))
  const [filter, setFilter] = useState("all")
  const [q, setQ] = useState("")
  const [detail, setDetail] = useState<string | null>(null) // server id
  const [creating, setCreating] = useState(false)
  const timers = useRef<Record<string, ReturnType<typeof setTimeout>>>({})

  const isOwner = role === "owner"
  const meId = "u-anya" // demo owner identity

  // patch one server by id
  const patch = useCallback((id: string, upd: Patch | ((s: Server) => Patch)) => {
    setServers((prev) =>
      prev.map((s) =>
        s.id === id ? { ...s, ...(typeof upd === "function" ? upd(s) : upd) } : s
      )
    )
  }, [])

  // step a server through a sequence of [status, ms, extra] then settle
  const runFlow = useCallback(
    (id: string, steps: Step[]) => {
      clearTimeout(timers.current[id])
      let i = 0
      const tick = () => {
        if (i >= steps.length) return
        const [status, ms, extra] = steps[i]
        i++
        patch(id, (s) => ({ status, ...(extra ? extra(s) : {}) }))
        timers.current[id] = setTimeout(tick, ms)
      }
      tick()
    },
    [patch]
  )

  const startServer = useCallback(
    (s: Server) => {
      const oversize = s.cpus > MAX_HOST_CPU || s.mem > MAX_HOST_MEM
      patch(s.id, { desired: "running", attempts: 0, statusMessage: null })
      if (oversize) {
        runFlow(s.id, [
          ["scheduling", 1100, () => ({ statusMessage: "Selecting host with capacity…" })],
          [
            "unschedulable",
            0,
            () => ({
              statusMessage: `No ready host fits ${s.cpus} vCPU / ${fmtMem(s.mem)} — largest host is ${MAX_HOST_CPU} vCPU / ${fmtMem(MAX_HOST_MEM)}.`,
            }),
          ],
        ])
        return
      }
      const ready = HOSTS.filter((h) => h.status === "ready")
      const host = ready[Math.floor(Math.random() * ready.length)]
      const port = 25565 + Math.floor(Math.random() * 40)
      runFlow(s.id, [
        ["scheduling", 1000, () => ({ statusMessage: "Selecting host with capacity…", hostId: null })],
        [
          "provisioning",
          1400,
          () => ({ statusMessage: `Placed on ${host.hostname} · booting microVM`, hostId: host.id }),
        ],
        ["starting", 1500, () => ({ statusMessage: "Pulling world archive · launching server" })],
        [
          "running",
          0,
          () => ({
            statusMessage: null,
            address: host.zone.startsWith("us-east") ? "play.craftling.gg" : host.address,
            port,
            health: "healthy",
            players: 0,
          }),
        ],
      ])
    },
    [patch, runFlow]
  )

  const stopServer = useCallback(
    (s: Server) => {
      patch(s.id, { desired: "stopped", statusMessage: null })
      runFlow(s.id, [
        ["stopping", 1300, () => ({ statusMessage: "RCON save-all · flushing world to object store" })],
        [
          "stopped",
          0,
          () => ({ statusMessage: null, hostId: null, address: null, port: null, players: 0, health: "—" }),
        ],
      ])
    },
    [patch, runFlow]
  )

  const restartServer = useCallback(
    (s: Server) => {
      runFlow(s.id, [
        ["stopping", 1100, () => ({ statusMessage: "Graceful stop" })],
        ["starting", 1500, () => ({ statusMessage: "Re-launching server" })],
        ["running", 0, () => ({ statusMessage: null, health: "healthy" })],
      ])
    },
    [runFlow]
  )

  const removeServer = useCallback((id: string) => {
    clearTimeout(timers.current[id])
    setServers((prev) => prev.filter((s) => s.id !== id))
    setDetail(null)
  }, [])

  const addServer = useCallback(
    (spec: CreateSpec) => {
      const s = srv({
        ...spec,
        owner: spec.owner || meId,
        desired: "running",
        status: "scheduling",
        hostId: null,
        players: 0,
        createdDays: 0,
        world: 0,
        statusMessage: "Selecting host with capacity…",
      })
      setServers((prev) => [s, ...prev])
      setCreating(false)
      setTimeout(() => startServer(s), 60)
    },
    [startServer]
  )

  // role + zone + filter + query scoping
  let scoped = servers
  if (isOwner) scoped = scoped.filter((s) => s.owner === meId)
  if (role === "operator" && zone !== "all")
    scoped = scoped.filter((s) => {
      const h = hostById(s.hostId)
      return h ? h.zone === zone : false
    })
  const fdef = FILTERS.find((f) => f.id === filter)!
  let list = scoped.filter((s) => filter === "all" || (fdef.match && fdef.match(s)))
  if (q.trim()) {
    const t = q.toLowerCase()
    list = list.filter(
      (s) =>
        s.name.toLowerCase().includes(t) ||
        (s.address || "").includes(t) ||
        s.version.includes(t)
    )
  }

  // stats
  const running = scoped.filter((s) => s.status === "running")
  const playersOnline = running.reduce((a, s) => a + s.players, 0)
  const usedCpu = scoped.filter((s) => s.hostId).reduce((a, s) => a + s.cpus, 0)
  const fleetCpu = HOSTS.filter((h) => h.status === "ready").reduce((a, h) => a + h.cpus, 0)

  useEffect(() => {
    if (onCountChange) onCountChange(scoped.length)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [scoped.length])
  useEffect(() => () => Object.values(timers.current).forEach(clearTimeout), [])

  const detailServer = detail ? servers.find((s) => s.id === detail) : null

  return (
    <div className="page-inner">
      <div className="page-head">
        <div>
          <div className="page-title">{isOwner ? "My Servers" : "Game Servers"}</div>
          <div className="page-sub">
            {isOwner
              ? "Your owned game servers — owner-scoped view."
              : "Fleet-wide desired vs. observed state, reconciled every 2s."}
          </div>
        </div>
        <Btn variant="primary" onClick={() => setCreating(true)}>
          <Icon name="plus" size={16} /> New Server
        </Btn>
      </div>

      {/* stat tiles */}
      <div style={{ display: "grid", gridTemplateColumns: "repeat(4, 1fr)", gap: 14 }}>
        <div className="card stat">
          <div className="k">
            <Icon name="cube" size={14} /> {isOwner ? "My servers" : "Total servers"}
          </div>
          <div className="v tnum">{scoped.length}</div>
          <div className="sub">
            {scoped.filter((s) => s.status === "stopped").length} stopped ·{" "}
            {scoped.filter((s) => ["error", "unschedulable"].includes(s.status)).length} need
            attention
          </div>
        </div>
        <div className="card stat">
          <div className="k">
            <i className="dot" style={{ background: "var(--success)" }} /> Running
          </div>
          <div className="v tnum" style={{ color: "var(--success-fg)" }}>
            {running.length}
          </div>
          <div className="sub">
            {
              scoped.filter((s) =>
                ["scheduling", "provisioning", "starting", "stopping"].includes(s.status)
              ).length
            }{" "}
            transitioning
          </div>
        </div>
        <div className="card stat">
          <div className="k">
            <Icon name="users" size={14} /> Players online
          </div>
          <div className="v tnum">{playersOnline}</div>
          <div className="sub">across {running.length} live servers</div>
        </div>
        <div className="card stat">
          <div className="k">
            <Icon name="cpu" size={14} /> {isOwner ? "Allocated vCPU" : "Fleet vCPU"}
          </div>
          <div className="v tnum">
            {usedCpu}
            {!isOwner && (
              <span className="muted" style={{ fontSize: 16, fontWeight: 500 }}>
                {" "}
                / {fleetCpu}
              </span>
            )}
          </div>
          {!isOwner && (
            <div style={{ marginTop: 2 }}>
              <Meter value={usedCpu} max={fleetCpu} />
            </div>
          )}
          {isOwner && <div className="sub">across your running servers</div>}
        </div>
      </div>

      {/* toolbar */}
      <div className="between" style={{ gap: 12, flexWrap: "wrap" }}>
        <div className="row gap-2 wrap">
          {FILTERS.map((f) => {
            const n =
              f.id === "all"
                ? scoped.length
                : scoped.filter((s) => f.match && f.match(s)).length
            return (
              <button
                key={f.id}
                className={"chip" + (filter === f.id ? " on" : "")}
                onClick={() => setFilter(f.id)}
              >
                {f.label} <span className="num">{n}</span>
              </button>
            )
          })}
        </div>
        <div className="input-wrap" style={{ width: 240 }}>
          <Icon name="search" />
          <input
            className="input"
            placeholder="Search name, version, address"
            value={q}
            onChange={(e) => setQ(e.target.value)}
          />
        </div>
      </div>

      {/* table */}
      <div className="card" style={{ overflow: "hidden" }}>
        <div style={{ overflowX: "auto" }}>
          <table className="tbl">
            <thead>
              <tr>
                <th>Server</th>
                <th>Status</th>
                <th>Version</th>
                {!isOwner && <th>Owner</th>}
                {!isOwner && <th>Host · Zone</th>}
                <th>Resources</th>
                <th>Players</th>
                <th>Address</th>
                <th className="actions" style={{ textAlign: "right" }}>
                  Actions
                </th>
              </tr>
            </thead>
            <tbody>
              {list.map((s) => (
                <ServerRow
                  key={s.id}
                  s={s}
                  isOwner={isOwner}
                  onOpen={() => setDetail(s.id)}
                  onStart={() => startServer(s)}
                  onStop={() => stopServer(s)}
                  onRestart={() => restartServer(s)}
                  onDelete={() => removeServer(s.id)}
                />
              ))}
            </tbody>
          </table>
        </div>
        {!list.length && (
          <div className="empty">
            <Icon name="cube" size={30} style={{ opacity: 0.5 }} />
            <div className="col" style={{ gap: 3, alignItems: "center" }}>
              <div className="semibold" style={{ color: "var(--foreground)" }}>
                No servers match
              </div>
              <div className="t-sm">Try a different filter or spin up a new server.</div>
            </div>
            <Btn variant="primary" size="sm" onClick={() => setCreating(true)}>
              <Icon name="plus" size={14} /> New Server
            </Btn>
          </div>
        )}
      </div>

      {detailServer && (
        <ServerDrawer
          s={detailServer}
          isOwner={isOwner}
          onClose={() => setDetail(null)}
          onStart={() => startServer(detailServer)}
          onStop={() => stopServer(detailServer)}
          onRestart={() => restartServer(detailServer)}
          onDelete={() => removeServer(detailServer.id)}
        />
      )}
      {creating && (
        <CreateDrawer role={role} onClose={() => setCreating(false)} onCreate={addServer} />
      )}
    </div>
  )
}

function ServerRow({
  s,
  isOwner,
  onOpen,
  onStart,
  onStop,
  onRestart,
  onDelete,
}: {
  s: Server
  isOwner: boolean
  onOpen: () => void
  onStart: () => void
  onStop: () => void
  onRestart: () => void
  onDelete: () => void
}) {
  const host = hostById(s.hostId)
  const owner = ownerById(s.owner)
  const transitioning = TRANSITIONING.includes(s.status)
  const canStart = CAN_START.includes(s.status)
  const canStop = CAN_STOP.includes(s.status)

  return (
    <tr className="selectable" onClick={onOpen}>
      <td>
        <div className="col" style={{ gap: 2 }}>
          <span className="semibold">{s.name}</span>
          <span className="mono t-xs muted">{s.id}</span>
        </div>
      </td>
      <td>
        <div className="col" style={{ gap: 4, alignItems: "flex-start" }}>
          <StatusBadge state={s.status} />
          {s.statusMessage && transitioning && (
            <span className="t-xs muted truncate" style={{ maxWidth: 230 }}>
              {s.statusMessage}
            </span>
          )}
          {s.status === "error" && (
            <span className="t-xs" style={{ color: "var(--danger-fg)" }}>
              attempt {s.attempts} · backing off
            </span>
          )}
        </div>
      </td>
      <td>
        <span className="badge mono">{s.version}</span>
      </td>
      {!isOwner && (
        <td>
          {owner ? (
            <div className="row gap-2">
              <div
                className="avatar"
                style={{ width: 24, height: 24, fontSize: 10, borderRadius: 5 }}
              >
                {owner.initials}
              </div>
              <span className="t-sm truncate" style={{ maxWidth: 110 }}>
                {owner.name}
              </span>
            </div>
          ) : (
            <span className="muted">—</span>
          )}
        </td>
      )}
      {!isOwner && (
        <td>
          {host ? (
            <div className="col" style={{ gap: 1 }}>
              <span className="mono t-sm">{host.hostname}</span>
              <span className="t-xs muted">{host.zone}</span>
            </div>
          ) : (
            <span className="muted">—</span>
          )}
        </td>
      )}
      <td>
        <div className="row gap-3 mono t-sm">
          <span className="row gap-1">
            <Icon name="cpu" size={13} style={{ color: "var(--muted-foreground)" }} />
            {s.cpus}
          </span>
          <span className="row gap-1">
            <Icon name="mem" size={13} style={{ color: "var(--muted-foreground)" }} />
            {fmtMem(s.mem)}
          </span>
        </div>
      </td>
      <td>
        {s.status === "running" ? (
          <div className="col" style={{ gap: 3, minWidth: 64 }}>
            <span className="mono t-sm tnum">
              {s.players}
              <span className="muted">/{s.maxPlayers}</span>
            </span>
            <Meter value={s.players} max={s.maxPlayers} />
          </div>
        ) : (
          <span className="muted">—</span>
        )}
      </td>
      <td>
        {s.address ? (
          <div className="row gap-1">
            <span className="mono t-sm truncate" style={{ maxWidth: 140 }}>
              {s.address}:{s.port}
            </span>
            <CopyBtn text={`${s.address}:${s.port}`} />
          </div>
        ) : (
          <span className="muted">—</span>
        )}
      </td>
      <td className="actions" onClick={(e) => e.stopPropagation()}>
        <div className="row gap-1" style={{ justifyContent: "flex-end" }}>
          {canStop ? (
            <Btn size="sm" variant="outline" onClick={onStop} disabled={s.status === "stopping"}>
              <Icon name="stop" size={13} /> Stop
            </Btn>
          ) : (
            <Btn size="sm" variant={canStart ? "primary" : "ghost"} onClick={onStart} disabled={!canStart}>
              <Icon name="play" size={13} /> Start
            </Btn>
          )}
          <Menu
            align="right"
            width={186}
            trigger={(_open, t) => (
              <button className="icon-btn sm" onClick={t}>
                <Icon name="more" size={16} />
              </button>
            )}
          >
            <div className="menu-item" onClick={onOpen}>
              <Icon name="server" /> View details
            </div>
            <div className="menu-item" onClick={onRestart}>
              <Icon name="restart" /> Restart
            </div>
            <div className="menu-item">
              <Icon name="download" /> Download world
            </div>
            <div className="menu-sep" />
            <div className="menu-item danger" onClick={onDelete}>
              <Icon name="trash" /> Delete server
            </div>
          </Menu>
        </div>
      </td>
    </tr>
  )
}
