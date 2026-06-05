/* servers-view.tsx — Game Servers view wired to the control-plane API:
 * list (owner- or fleet-scoped), lifecycle actions, and live status polling. */
import { useCallback, useEffect, useRef, useState } from "react"
import { Icon } from "./icon"
import { Btn, CopyBtn, Menu, Meter, StatusBadge } from "./primitives"
import { ServerDrawer, CreateDrawer, type CreateSpec } from "./drawers"
import { FILTERS } from "./servers-shared"
import { api, toServer, ApiError, type ApiServer } from "@/lib/api"
import { useAuth } from "@/lib/auth"
import { fmtMem, type Owner, type Role, type Server, type ServerStatus } from "@/lib/data"

const TRANSITIONING: ServerStatus[] = ["scheduling", "provisioning", "starting", "stopping"]
const CAN_START: ServerStatus[] = ["stopped", "error", "unschedulable"]
const CAN_STOP: ServerStatus[] = ["running", "starting", "provisioning"]

const POLL_MS = 2500

// While an optimistic action is in flight we hold a transitioning label until
// the backend's observed status actually moves off `from`.
interface Pending {
  show: ServerStatus
  from: ServerStatus
}

function ownerFromEmail(id: string, email: string): Owner {
  const local = email.split("@")[0]
  const initials = (local.replace(/[^a-z0-9]/gi, "").slice(0, 2) || "??").toUpperCase()
  return { id, name: email, email, initials }
}

export function ServersView({
  role,
  onCountChange,
}: {
  role: Role
  onCountChange?: (n: number) => void
}) {
  const { user } = useAuth()
  const isOwner = role === "owner"

  const [servers, setServers] = useState<Server[]>([])
  const [owners, setOwners] = useState<Record<string, Owner>>({})
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [filter, setFilter] = useState("all")
  const [q, setQ] = useState("")
  const [detail, setDetail] = useState<string | null>(null) // server id
  const [creating, setCreating] = useState(false)

  const pending = useRef<Map<string, Pending>>(new Map())
  const removed = useRef<Set<string>>(new Set())

  // Merge fresh API data with any in-flight optimistic state.
  const applyServers = useCallback((fresh: ApiServer[]) => {
    setServers(
      fresh
        .filter((a) => !removed.current.has(a.id))
        .map((a) => {
          const s = toServer(a)
          const p = pending.current.get(s.id)
          if (p) {
            if (s.status === p.from) return { ...s, status: p.show }
            pending.current.delete(s.id) // backend has moved on
          }
          return s
        })
    )
  }, [])

  // Fetch the current scope's servers (+ owner directory for the fleet view).
  const fetchScope = useCallback(async () => {
    if (isOwner) {
      const list = await api.listServers()
      const dir = user ? { [user.id]: ownerFromEmail(user.id, user.email) } : {}
      return { servers: list, owners: dir as Record<string, Owner> }
    }
    const [list, users] = await Promise.all([api.adminListServers(), api.adminListUsers()])
    return {
      servers: list,
      owners: Object.fromEntries(users.map((u) => [u.id, ownerFromEmail(u.id, u.email)])),
    }
  }, [isOwner, user])

  // Sync wrapper: all state updates happen in promise callbacks, so this is safe
  // to call from an effect (no synchronous setState in the effect body).
  const refresh = useCallback(() => {
    fetchScope()
      .then(({ servers: s, owners: o }) => {
        setOwners(o)
        applyServers(s)
        setError(null)
      })
      .catch((e) =>
        setError(e instanceof ApiError ? e.message : "Couldn't reach the control plane.")
      )
      .finally(() => setLoading(false))
  }, [fetchScope, applyServers])

  // Initial load + steady poll. Re-runs if the scope (role/user) changes.
  useEffect(() => {
    refresh()
    const t = setInterval(refresh, POLL_MS)
    return () => clearInterval(t)
  }, [refresh])

  // Optimistically show `show` for a server, holding until the backend leaves `from`.
  const optimistic = useCallback((s: Server, show: ServerStatus, statusMessage: string | null) => {
    pending.current.set(s.id, { show, from: s.status })
    setServers((prev) => prev.map((x) => (x.id === s.id ? { ...x, status: show, statusMessage } : x)))
  }, [])

  const runAction = useCallback(
    async (fn: () => Promise<unknown>) => {
      try {
        await fn()
      } catch (e) {
        setError(e instanceof ApiError ? e.message : "Action failed.")
      } finally {
        refresh()
      }
    },
    [refresh]
  )

  const startServer = useCallback(
    (s: Server) => {
      optimistic(s, "scheduling", "Selecting host with capacity…")
      runAction(() => api.updateServer(s.id, { desired_state: "running" }))
    },
    [optimistic, runAction]
  )

  const stopServer = useCallback(
    (s: Server) => {
      optimistic(s, "stopping", "Draining and tearing down microVM…")
      runAction(() => api.updateServer(s.id, { desired_state: "stopped" }))
    },
    [optimistic, runAction]
  )

  const restartServer = useCallback(
    (s: Server) => {
      optimistic(s, "stopping", "Restarting…")
      runAction(() => api.restartServer(s.id))
    },
    [optimistic, runAction]
  )

  const removeServer = useCallback(
    (id: string) => {
      removed.current.add(id)
      pending.current.delete(id)
      setServers((prev) => prev.filter((s) => s.id !== id))
      setDetail(null)
      runAction(() => api.deleteServer(id))
    },
    [runAction]
  )

  const addServer = useCallback(
    (spec: CreateSpec) => {
      setCreating(false)
      runAction(() =>
        api.createServer({
          name: spec.name,
          version: spec.version,
          cpus: spec.cpus,
          memory_mb: spec.mem,
        })
      )
    },
    [runAction]
  )

  // filter + query scoping (role scoping is enforced server-side)
  const fdef = FILTERS.find((f) => f.id === filter)!
  let list = servers.filter((s) => filter === "all" || (fdef.match && fdef.match(s)))
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
  const running = servers.filter((s) => s.status === "running")
  const memAllocated = running.reduce((a, s) => a + s.mem, 0)
  const cpuAllocated = running.reduce((a, s) => a + s.cpus, 0)

  useEffect(() => {
    onCountChange?.(servers.length)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [servers.length])

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

      {error && (
        <div
          className="row gap-2 t-sm"
          style={{
            color: "var(--danger-fg)",
            background: "color-mix(in oklab, var(--danger) 10%, transparent)",
            padding: "10px 12px",
            borderRadius: "var(--radius)",
            alignItems: "center",
          }}
        >
          <Icon name="alert" size={15} style={{ flex: "none" }} />
          <span>{error}</span>
          <button className="icon-btn sm" onClick={refresh} style={{ marginLeft: "auto" }}>
            <Icon name="restart" size={14} />
          </button>
        </div>
      )}

      {/* stat tiles */}
      <div style={{ display: "grid", gridTemplateColumns: "repeat(4, 1fr)", gap: 14 }}>
        <div className="card stat">
          <div className="k">
            <Icon name="cube" size={14} /> {isOwner ? "My servers" : "Total servers"}
          </div>
          <div className="v tnum">{servers.length}</div>
          <div className="sub">
            {servers.filter((s) => s.status === "stopped").length} stopped ·{" "}
            {servers.filter((s) => ["error", "unschedulable"].includes(s.status)).length} need
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
            {servers.filter((s) => TRANSITIONING.includes(s.status)).length} transitioning
          </div>
        </div>
        <div className="card stat">
          <div className="k">
            <Icon name="cpu" size={14} /> Allocated vCPU
          </div>
          <div className="v tnum">{cpuAllocated}</div>
          <div className="sub">across {running.length} running servers</div>
        </div>
        <div className="card stat">
          <div className="k">
            <Icon name="mem" size={14} /> Allocated memory
          </div>
          <div className="v tnum">{fmtMem(memAllocated)}</div>
          <div className="sub">across {running.length} running servers</div>
        </div>
      </div>

      {/* toolbar */}
      <div className="between" style={{ gap: 12, flexWrap: "wrap" }}>
        <div className="row gap-2 wrap">
          {FILTERS.map((f) => {
            const n =
              f.id === "all"
                ? servers.length
                : servers.filter((s) => f.match && f.match(s)).length
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
                  owner={owners[s.owner]}
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
            {loading ? (
              <>
                <Icon name="restart" className="spin" size={26} style={{ opacity: 0.6 }} />
                <div className="t-sm">Loading servers…</div>
              </>
            ) : (
              <>
                <Icon name="cube" size={30} style={{ opacity: 0.5 }} />
                <div className="col" style={{ gap: 3, alignItems: "center" }}>
                  <div className="semibold" style={{ color: "var(--foreground)" }}>
                    {servers.length ? "No servers match" : "No servers yet"}
                  </div>
                  <div className="t-sm">
                    {servers.length
                      ? "Try a different filter or search."
                      : "Spin up your first game server."}
                  </div>
                </div>
                <Btn variant="primary" size="sm" onClick={() => setCreating(true)}>
                  <Icon name="plus" size={14} /> New Server
                </Btn>
              </>
            )}
          </div>
        )}
      </div>

      {detailServer && (
        <ServerDrawer
          s={detailServer}
          isOwner={isOwner}
          owner={owners[detailServer.owner]}
          onClose={() => setDetail(null)}
          onStart={() => startServer(detailServer)}
          onStop={() => stopServer(detailServer)}
          onRestart={() => restartServer(detailServer)}
          onDelete={() => removeServer(detailServer.id)}
        />
      )}
      {creating && <CreateDrawer onClose={() => setCreating(false)} onCreate={addServer} />}
    </div>
  )
}

function ServerRow({
  s,
  isOwner,
  owner,
  onOpen,
  onStart,
  onStop,
  onRestart,
  onDelete,
}: {
  s: Server
  isOwner: boolean
  owner?: Owner | null
  onOpen: () => void
  onStart: () => void
  onStop: () => void
  onRestart: () => void
  onDelete: () => void
}) {
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
          {s.status === "error" && s.statusMessage && (
            <span className="t-xs truncate" style={{ color: "var(--danger-fg)", maxWidth: 230 }}>
              {s.statusMessage}
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
              <span className="t-sm truncate" style={{ maxWidth: 140 }}>
                {owner.name}
              </span>
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
        {s.status === "running" && s.maxPlayers > 0 ? (
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
