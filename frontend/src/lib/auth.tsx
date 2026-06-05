/* auth.tsx — authentication context backed by the control-plane API.
 *
 * Holds the current user, exposes login/register/logout, and maps the backend
 * role (user | admin) onto the UI's role model (owner | operator). */
import { createContext, useCallback, useContext, useEffect, useState, type ReactNode } from "react"
import { api, tokenStore, type ApiUser } from "./api"
import type { Role } from "./data"

/** admin → operator (fleet-wide), user → owner (owner-scoped). */
// eslint-disable-next-line react-refresh/only-export-components
export const roleFor = (u: ApiUser): Role => (u.role === "admin" ? "operator" : "owner")

interface AuthState {
  user: ApiUser | null
  role: Role
  loading: boolean
  login: (email: string, password: string) => Promise<void>
  register: (email: string, password: string) => Promise<void>
  logout: () => Promise<void>
}

const AuthContext = createContext<AuthState | null>(null)

export function AuthProvider({ children }: { children: ReactNode }) {
  const [user, setUser] = useState<ApiUser | null>(null)
  // Resolve an existing session (if any) before the first paint. Only "loading"
  // when there's actually a stored token to validate.
  const [loading, setLoading] = useState(() => Boolean(tokenStore.access || tokenStore.refresh))

  useEffect(() => {
    if (!tokenStore.access && !tokenStore.refresh) return
    let active = true
    api
      .me()
      .then((u) => active && setUser(u))
      .catch(() => {
        tokenStore.clear()
        if (active) setUser(null)
      })
      .finally(() => active && setLoading(false))
    return () => {
      active = false
    }
  }, [])

  const login = useCallback(async (email: string, password: string) => {
    await api.login(email, password)
    setUser(await api.me())
  }, [])

  const register = useCallback(async (email: string, password: string) => {
    await api.register(email, password)
    setUser(await api.me())
  }, [])

  const logout = useCallback(async () => {
    await api.logout()
    setUser(null)
  }, [])

  const role: Role = user ? roleFor(user) : "owner"

  return (
    <AuthContext.Provider value={{ user, role, loading, login, register, logout }}>
      {children}
    </AuthContext.Provider>
  )
}

// eslint-disable-next-line react-refresh/only-export-components
export function useAuth(): AuthState {
  const ctx = useContext(AuthContext)
  if (!ctx) throw new Error("useAuth must be used within AuthProvider")
  return ctx
}
