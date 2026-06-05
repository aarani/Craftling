/* auth-screen.tsx — sign-in / create-account screen wired to the control plane. */
import { useState } from "react"
import { Icon, Voxel } from "./icon"
import { Btn } from "./primitives"
import { useAuth } from "@/lib/auth"
import { ApiError } from "@/lib/api"

type Mode = "login" | "register"

export function AuthScreen({
  theme,
  toggleTheme,
}: {
  theme: "dark" | "light"
  toggleTheme: () => void
}) {
  const { login, register } = useAuth()
  const [mode, setMode] = useState<Mode>("login")
  const [email, setEmail] = useState("")
  const [pw, setPw] = useState("")
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    setError(null)
    setBusy(true)
    try {
      if (mode === "register") await register(email.trim(), pw)
      else await login(email.trim(), pw)
    } catch (err) {
      setError(
        err instanceof ApiError
          ? err.message
          : "Couldn't reach the control plane. Is the server running?"
      )
      setBusy(false)
    }
  }

  return (
    <div
      style={{
        minHeight: "100%",
        display: "grid",
        gridTemplateColumns: "1fr",
        placeItems: "center",
        position: "relative",
        padding: 24,
      }}
    >
      <button
        className="icon-btn"
        onClick={toggleTheme}
        title="Toggle theme"
        style={{ position: "absolute", top: 18, right: 18 }}
      >
        <Icon name={theme === "dark" ? "sun" : "moon"} />
      </button>

      {/* faint voxel grid backdrop */}
      <div
        aria-hidden
        style={{
          position: "absolute",
          inset: 0,
          pointerEvents: "none",
          opacity: 0.5,
          backgroundImage:
            "linear-gradient(var(--border) 1px, transparent 1px), linear-gradient(90deg, var(--border) 1px, transparent 1px)",
          backgroundSize: "34px 34px",
          maskImage: "radial-gradient(circle at 50% 40%, black, transparent 72%)",
          WebkitMaskImage:
            "radial-gradient(circle at 50% 40%, black, transparent 72%)",
        }}
      />

      <div
        className="col"
        style={{
          width: 380,
          maxWidth: "100%",
          gap: 22,
          position: "relative",
          zIndex: 1,
        }}
      >
        <div
          className="col"
          style={{ gap: 14, alignItems: "center", textAlign: "center" }}
        >
          <Voxel className="lg" />
          <div style={{ fontSize: 25, fontWeight: 700, letterSpacing: "-0.02em" }}>
            Craftling
          </div>
        </div>

        <form
          className="card"
          style={{ padding: 22, display: "flex", flexDirection: "column", gap: 16 }}
          onSubmit={submit}
        >
          <div className="seg" style={{ width: "100%" }}>
            <button
              type="button"
              className={mode === "login" ? "on" : ""}
              style={{ flex: 1, justifyContent: "center" }}
              onClick={() => {
                setMode("login")
                setError(null)
              }}
            >
              <Icon name="shield" /> Sign in
            </button>
            <button
              type="button"
              className={mode === "register" ? "on" : ""}
              style={{ flex: 1, justifyContent: "center" }}
              onClick={() => {
                setMode("register")
                setError(null)
              }}
            >
              <Icon name="user" /> Create account
            </button>
          </div>

          <div className="field">
            <label className="label">Email</label>
            <input
              className="input"
              type="email"
              value={email}
              onChange={(e) => setEmail(e.target.value)}
              placeholder="you@craftling.gg"
              autoComplete="username"
              required
            />
          </div>
          <div className="field">
            <div className="between">
              <label className="label">Password</label>
              {mode === "register" && (
                <span className="t-xs muted">Min 8 characters</span>
              )}
            </div>
            <input
              className="input"
              type="password"
              value={pw}
              onChange={(e) => setPw(e.target.value)}
              placeholder="••••••••••"
              autoComplete={mode === "register" ? "new-password" : "current-password"}
              minLength={8}
              required
            />
          </div>

          {error && (
            <div
              className="row gap-2 t-sm"
              style={{
                color: "var(--danger-fg)",
                background: "color-mix(in oklab, var(--danger) 10%, transparent)",
                padding: "9px 11px",
                borderRadius: "var(--radius)",
                alignItems: "flex-start",
              }}
            >
              <Icon name="alert" size={15} style={{ marginTop: 1, flex: "none" }} />
              <span>{error}</span>
            </div>
          )}

          <Btn variant="primary" size="lg" type="submit" className="block" disabled={busy}>
            {busy ? (
              <>
                <Icon name="restart" className="spin" size={16} />{" "}
                {mode === "register" ? "Creating account…" : "Signing in…"}
              </>
            ) : mode === "register" ? (
              <>Create account</>
            ) : (
              <>Sign in</>
            )}
          </Btn>
        </form>

        <div
          className="row"
          style={{ gap: 8, justifyContent: "center", color: "var(--muted-foreground)" }}
        >
          <span className="badge">
            <i className="dot" style={{ background: "var(--success)" }} /> All systems
            operational
          </span>
        </div>
      </div>
    </div>
  )
}
