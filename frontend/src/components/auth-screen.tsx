/* auth-screen.tsx — sign-in screen with role demo + theme toggle. */
import { useState } from "react"
import { Icon, Voxel } from "./icon"
import { Btn } from "./primitives"
import type { Role } from "@/lib/data"

const roleEmail = (r: Role) => (r === "operator" ? "ops@craftling.gg" : "anya@craftling.gg")

export function AuthScreen({
  onSignIn,
  theme,
  toggleTheme,
}: {
  onSignIn: (role: Role) => void
  theme: "dark" | "light"
  toggleTheme: () => void
}) {
  const [role, setRole] = useState<Role>("operator")
  const [email, setEmail] = useState(roleEmail("operator"))
  const [pw, setPw] = useState("••••••••••")
  const [busy, setBusy] = useState(false)

  // Switching role swaps in that role's demo email (keeps any manual edit otherwise).
  const pickRole = (r: Role) => {
    setRole(r)
    setEmail(roleEmail(r))
  }

  const submit = (e: React.FormEvent) => {
    e.preventDefault()
    setBusy(true)
    setTimeout(() => onSignIn(role), 620)
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
          <div className="col" style={{ gap: 5 }}>
            <div style={{ fontSize: 25, fontWeight: 700, letterSpacing: "-0.02em" }}>
              Craftling
            </div>
            <div className="muted t-sm">
              Firecracker microVM hosting · control plane
            </div>
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
              className={role === "operator" ? "on" : ""}
              style={{ flex: 1, justifyContent: "center" }}
              onClick={() => pickRole("operator")}
            >
              <Icon name="shield" /> Operator
            </button>
            <button
              type="button"
              className={role === "owner" ? "on" : ""}
              style={{ flex: 1, justifyContent: "center" }}
              onClick={() => pickRole("owner")}
            >
              <Icon name="user" /> Owner
            </button>
          </div>

          <div className="field">
            <label className="label">Email</label>
            <input
              className="input"
              type="email"
              value={email}
              onChange={(e) => setEmail(e.target.value)}
              autoComplete="username"
            />
          </div>
          <div className="field">
            <div className="between">
              <label className="label">Password</label>
              <a
                className="t-xs"
                style={{
                  color: "var(--primary)",
                  textDecoration: "none",
                  fontWeight: 540,
                }}
                href="#"
                onClick={(e) => e.preventDefault()}
              >
                Forgot?
              </a>
            </div>
            <input
              className="input"
              type="password"
              value={pw}
              onChange={(e) => setPw(e.target.value)}
              autoComplete="current-password"
            />
          </div>

          <Btn variant="primary" size="lg" type="submit" className="block" disabled={busy}>
            {busy ? (
              <>
                <Icon name="restart" className="spin" size={16} /> Signing in…
              </>
            ) : (
              <>Sign in</>
            )}
          </Btn>

          <div className="t-xs muted" style={{ textAlign: "center", lineHeight: 1.6 }}>
            Demo ·{" "}
            <b style={{ color: "var(--foreground)" }}>
              {role === "operator" ? "Operator" : "Owner"}
            </b>{" "}
            sees{" "}
            {role === "operator" ? "the whole fleet" : "only their own servers"}. JWT +
            rotating refresh tokens.
          </div>
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
