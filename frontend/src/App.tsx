/* App.tsx — root: auth gate, theme, role, routing. */
import { useEffect, useState } from "react"
import { AppShell, type Route } from "@/components/app-shell"
import { AuthScreen } from "@/components/auth-screen"
import { ServersView } from "@/components/servers-view"
import { StubView } from "@/components/stub-view"
import { usePersist } from "@/lib/use-persist"
import { HOSTS, SERVERS, type Role as UserRole } from "@/lib/data"

export function App() {
  const [theme, setTheme] = usePersist<"dark" | "light">("cl-theme", "dark")
  const [authed, setAuthed] = usePersist<boolean>("cl-authed", false)
  const [role, setRole] = usePersist<UserRole>("cl-role", "operator")
  const [route, setRoute] = usePersist<Route>("cl-route", "servers")
  const [zone, setZone] = useState("all")
  const [serverCount, setServerCount] = useState(SERVERS.length)

  useEffect(() => {
    document.documentElement.classList.toggle("dark", theme === "dark")
  }, [theme])
  const toggleTheme = () => setTheme((x) => (x === "dark" ? "light" : "dark"))

  useEffect(() => {
    if (role === "owner" && ["hosts", "scheduler", "quotas"].includes(route)) setRoute("servers")
  }, [role, route, setRoute])

  const signIn = (r: UserRole) => {
    setRole(r)
    setRoute("servers")
    setAuthed(true)
  }
  const signOut = () => setAuthed(false)

  if (!authed) {
    return <AuthScreen onSignIn={signIn} theme={theme} toggleTheme={toggleTheme} />
  }

  const counts: Partial<Record<Route, number>> = {
    servers:
      role === "owner" ? SERVERS.filter((s) => s.owner === "u-anya").length : serverCount,
    hosts: HOSTS.length,
  }

  return (
    <AppShell
      route={route}
      setRoute={setRoute}
      role={role}
      setRole={setRole}
      theme={theme}
      toggleTheme={toggleTheme}
      onSignOut={signOut}
      counts={counts}
      zone={zone}
      setZone={setZone}
    >
      {route === "servers" ? (
        <ServersView role={role} zone={zone} onCountChange={setServerCount} />
      ) : (
        <StubView route={route} />
      )}
    </AppShell>
  )
}

export default App
