/* marketplace-view.tsx — registry gallery. Lists templates from the control
 * plane, and on selection opens the dynamic config form. Stops at resolving the
 * env (the seam for the future init/rootfs + create-server step). */
import { useCallback, useEffect, useState } from "react"
import { Icon } from "./icon"
import { Btn } from "./primitives"
import { TemplateDrawer, type TemplateLaunch } from "./template-drawer"
import { api, ApiError, type TemplateSummary } from "@/lib/api"

export function MarketplaceView() {
  const [templates, setTemplates] = useState<TemplateSummary[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [selected, setSelected] = useState<TemplateSummary | null>(null)
  const [launched, setLaunched] = useState<TemplateLaunch | null>(null)

  // All state updates happen in promise callbacks so this is safe to call from
  // an effect (no synchronous setState in the effect body).
  const load = useCallback(() => {
    api
      .listTemplates()
      .then((t) => {
        setTemplates(t)
        setError(null)
      })
      .catch((e) =>
        setError(e instanceof ApiError ? e.message : "Couldn't reach the template registry.")
      )
      .finally(() => setLoading(false))
  }, [])

  useEffect(() => {
    load()
  }, [load])

  // Stops here per scope: surface the resolved env; provisioning comes later.
  const onComplete = useCallback((launch: TemplateLaunch) => {
    console.log("[marketplace] resolved launch", launch)
    setSelected(null)
    setLaunched(launch)
  }, [])

  return (
    <div className="page-inner">
      <div className="page-head">
        <div>
          <div className="page-title">Marketplace</div>
          <div className="page-sub">
            Launch a game server from a template. Answer its questions and Craftling fills the
            container environment.
          </div>
        </div>
        <Btn variant="outline" onClick={load}>
          <Icon name="refresh" size={15} /> Refresh
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
          <button className="icon-btn sm" onClick={load} style={{ marginLeft: "auto" }}>
            <Icon name="restart" size={14} />
          </button>
        </div>
      )}

      {launched && (
        <div
          className="row gap-2 t-sm"
          style={{
            color: "var(--success-fg)",
            background: "color-mix(in oklab, var(--success) 12%, transparent)",
            padding: "10px 12px",
            borderRadius: "var(--radius)",
            alignItems: "center",
          }}
        >
          <Icon name="check" size={15} style={{ flex: "none" }} />
          <span>
            <b>{launched.name}</b> configured from <b>{launched.manifest.template_name}</b> —{" "}
            {Object.keys(launched.env).length} env vars resolved. Provisioning hand-off is next.
          </span>
          <button
            className="icon-btn sm"
            onClick={() => setLaunched(null)}
            style={{ marginLeft: "auto" }}
          >
            <Icon name="x" size={14} />
          </button>
        </div>
      )}

      <div
        style={{
          display: "grid",
          gridTemplateColumns: "repeat(auto-fill, minmax(240px, 1fr))",
          gap: 14,
        }}
      >
        {templates.map((t) => (
          <button
            key={t.template_id}
            className="card selectable"
            onClick={() => setSelected(t)}
            style={{
              padding: 0,
              overflow: "hidden",
              textAlign: "left",
              cursor: "pointer",
              display: "flex",
              flexDirection: "column",
            }}
          >
            <div
              style={{
                aspectRatio: "16 / 9",
                background: "var(--muted)",
                display: "grid",
                placeItems: "center",
              }}
            >
              {t.thumbnail_url ? (
                <img
                  src={t.thumbnail_url}
                  alt=""
                  style={{ width: "100%", height: "100%", objectFit: "cover" }}
                />
              ) : (
                <Icon name="cube" size={28} style={{ opacity: 0.5 }} />
              )}
            </div>
            <div className="between" style={{ padding: "11px 13px", alignItems: "center" }}>
              <span className="semibold t-sm">{t.template_name}</span>
              <Icon name="chevRight" size={16} style={{ color: "var(--muted-foreground)" }} />
            </div>
          </button>
        ))}
      </div>

      {!templates.length && (
        <div className="card">
          <div className="empty">
            {loading ? (
              <>
                <Icon name="restart" className="spin" size={26} style={{ opacity: 0.6 }} />
                <div className="t-sm">Loading templates…</div>
              </>
            ) : (
              <>
                <Icon name="globe" size={30} style={{ opacity: 0.5 }} />
                <div className="col" style={{ gap: 3, alignItems: "center" }}>
                  <div className="semibold" style={{ color: "var(--foreground)" }}>
                    No templates available
                  </div>
                  <div className="t-sm">The registry index returned no entries.</div>
                </div>
              </>
            )}
          </div>
        </div>
      )}

      {selected && (
        <TemplateDrawer
          summary={selected}
          onClose={() => setSelected(null)}
          onComplete={onComplete}
        />
      )}
    </div>
  )
}
