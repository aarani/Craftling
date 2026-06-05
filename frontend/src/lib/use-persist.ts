import { useEffect, useState } from "react"

/* localStorage-backed state, mirroring the design's usePersist. */
export function usePersist<T>(key: string, init: T) {
  const [v, setV] = useState<T>(() => {
    try {
      const s = localStorage.getItem(key)
      return s == null ? init : (JSON.parse(s) as T)
    } catch {
      return init
    }
  })
  useEffect(() => {
    try {
      localStorage.setItem(key, JSON.stringify(v))
    } catch {
      /* ignore */
    }
  }, [key, v])
  return [v, setV] as const
}
