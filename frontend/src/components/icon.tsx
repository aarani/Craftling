/* icon.tsx — line-icon set (lucide-ish) + isometric voxel logo. */
/* eslint-disable react-refresh/only-export-components */
import type { CSSProperties } from "react"

/* ---------- icon set (lucide-ish, simple line paths) ---------- */
export const ICON = {
  server: "M3 5.5h18M3 5.5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2v3a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2zM3 15.5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2v3a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2zM7 7h.01M7 17h.01",
  cube: "M12 2.5l8.5 4.9v9.2L12 21.5l-8.5-4.9V7.4zM3.8 7.3 12 12m0 0 8.2-4.7M12 12v9.5",
  hosts: "M5 3.5h14a1.5 1.5 0 0 1 1.5 1.5v4a1.5 1.5 0 0 1-1.5 1.5H5A1.5 1.5 0 0 1 3.5 9V5A1.5 1.5 0 0 1 5 3.5zM5 13.5h14a1.5 1.5 0 0 1 1.5 1.5v4a1.5 1.5 0 0 1-1.5 1.5H5A1.5 1.5 0 0 1 3.5 19v-4A1.5 1.5 0 0 1 5 13.5zM7.5 6.5h.01M7.5 16.5h.01",
  schedule: "M8 2.5v3M16 2.5v3M4 8.5h16M5 4.5h14a1.5 1.5 0 0 1 1.5 1.5V19a1.5 1.5 0 0 1-1.5 1.5H5A1.5 1.5 0 0 1 3.5 19V6A1.5 1.5 0 0 1 5 4.5zM12.5 12.5l-2 2.5h3l-2 2.5",
  activity: "M3 12h4l2.5-7 4 16 2.5-9H21",
  shield: "M12 2.7l7 2.6v5.4c0 4.3-2.9 7.6-7 9-4.1-1.4-7-4.7-7-9V5.3z",
  users: "M16.5 20v-1.5a3.5 3.5 0 0 0-3.5-3.5H7a3.5 3.5 0 0 0-3.5 3.5V20M10 11.5a3.25 3.25 0 1 0 0-6.5 3.25 3.25 0 0 0 0 6.5M20.5 20v-1.5a3.5 3.5 0 0 0-2.6-3.4M15.5 5.2a3.25 3.25 0 0 1 0 6.1",
  settings: "M12 15.4a3.4 3.4 0 1 0 0-6.8 3.4 3.4 0 0 0 0 6.8zM19.4 15a1.5 1.5 0 0 0 .3 1.65l.05.05a1.82 1.82 0 1 1-2.57 2.57l-.05-.05a1.5 1.5 0 0 0-2.55 1.06V21a1.82 1.82 0 0 1-3.64 0v-.09A1.5 1.5 0 0 0 9.6 19.5a1.5 1.5 0 0 0-1.65.3l-.05.05a1.82 1.82 0 1 1-2.57-2.57l.05-.05a1.5 1.5 0 0 0-1.06-2.55H4.2a1.82 1.82 0 0 1 0-3.64h.09A1.5 1.5 0 0 0 5.6 9.6a1.5 1.5 0 0 0-.3-1.65l-.05-.05a1.82 1.82 0 1 1 2.57-2.57l.05.05A1.5 1.5 0 0 0 9.6 5.6h.07A1.5 1.5 0 0 0 10.7 4.2V4.1a1.82 1.82 0 0 1 3.64 0v.09a1.5 1.5 0 0 0 .9 1.37 1.5 1.5 0 0 0 1.65-.3l.05-.05a1.82 1.82 0 1 1 2.57 2.57l-.05.05a1.5 1.5 0 0 0-.3 1.65v.07a1.5 1.5 0 0 0 1.37.9h.1a1.82 1.82 0 0 1 0 3.64h-.09a1.5 1.5 0 0 0-1.37.9z",
  play: "M7 4.5l12 7.5-12 7.5z",
  stop: "M6.5 6.5h11v11h-11z",
  restart: "M21 3v6h-6M3 21v-6h6M20.5 14a8.5 8.5 0 0 1-15.6 3.5M3.5 10A8.5 8.5 0 0 1 19.1 6.5",
  plus: "M12 5v14M5 12h14",
  search: "M11 19a8 8 0 1 0 0-16 8 8 0 0 0 0 16zM21 21l-4.3-4.3",
  sun: "M12 17a5 5 0 1 0 0-10 5 5 0 0 0 0 10zM12 1.5v2.5M12 20v2.5M4.2 4.2l1.8 1.8M18 18l1.8 1.8M1.5 12H4M20 12h2.5M4.2 19.8 6 18M18 6l1.8-1.8",
  moon: "M21 12.8A9 9 0 1 1 11.2 3a7 7 0 0 0 9.8 9.8z",
  chevDown: "M5 8.5l7 7 7-7",
  chevRight: "M9 5l7 7-7 7",
  logout: "M15 4.5h2.5A1.5 1.5 0 0 1 19 6v12a1.5 1.5 0 0 1-1.5 1.5H15M10 16.5 14.5 12 10 7.5M14 12H3.5",
  alert: "M12 9v4M12 17h.01M10.3 3.9 2.4 17.5A2 2 0 0 0 4.1 20.5h15.8a2 2 0 0 0 1.7-3L13.7 3.9a2 2 0 0 0-3.4 0z",
  more: "M12 12h.01M19 12h.01M5 12h.01",
  globe: "M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18zM3.5 12h17M12 3a13 13 0 0 1 0 18 13 13 0 0 1 0-18z",
  cpu: "M7 7.5h10v9H7zM9 2.5v2M15 2.5v2M9 19.5v2M15 19.5v2M2.5 9h2M2.5 15h2M19.5 9h2M19.5 15h2",
  mem: "M5 6.5h14a1.5 1.5 0 0 1 1.5 1.5v8a1.5 1.5 0 0 1-1.5 1.5H5A1.5 1.5 0 0 1 3.5 16V8A1.5 1.5 0 0 1 5 6.5zM7 10v3M11 10v3M15 10v3M19 10v3",
  user: "M12 12.5a4 4 0 1 0 0-8 4 4 0 0 0 0 8zM5 20v-.5a5.5 5.5 0 0 1 5.5-5.5h3a5.5 5.5 0 0 1 5.5 5.5v.5",
  check: "M5 12.5l4.5 4.5L19.5 7",
  copy: "M9 9h9.5a1.5 1.5 0 0 1 1.5 1.5V20a1.5 1.5 0 0 1-1.5 1.5H9A1.5 1.5 0 0 1 7.5 20V10.5A1.5 1.5 0 0 1 9 9zM4.5 15H4a1.5 1.5 0 0 1-1.5-1.5V4A1.5 1.5 0 0 1 4 2.5h9.5A1.5 1.5 0 0 1 15 4v.5",
  link: "M10 13.5a3.5 3.5 0 0 0 5 .2l2.5-2.5a3.54 3.54 0 0 0-5-5l-1.4 1.4M14 10.5a3.5 3.5 0 0 0-5-.2L6.5 12.8a3.54 3.54 0 0 0 5 5l1.4-1.4",
  bolt: "M13 2.5 4 13.5h7l-1 8 9-11h-7z",
  gauge: "M12 21a9 9 0 1 1 0-18 9 9 0 0 1 0 18zM12 13l4-4M12 13a1.5 1.5 0 1 0 0-3 1.5 1.5 0 0 0 0 3z",
  filter: "M3.5 5.5h17l-6.5 7.5V19l-4 2v-7.5z",
  x: "M6 6l12 12M18 6 6 18",
  trash: "M4 6.5h16M9 6.5V5a1.5 1.5 0 0 1 1.5-1.5h3A1.5 1.5 0 0 1 15 5v1.5M6.5 6.5 7.3 19a1.5 1.5 0 0 0 1.5 1.4h6.4a1.5 1.5 0 0 0 1.5-1.4l.8-12.5",
  clock: "M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18zM12 7.5V12l3 2",
  download: "M12 3v12m0 0 4.5-4.5M12 15l-4.5-4.5M4.5 19.5h15",
  pin: "M12 21s7-5.2 7-11a7 7 0 1 0-14 0c0 5.8 7 11 7 11zM12 12.5a2.5 2.5 0 1 0 0-5 2.5 2.5 0 0 0 0 5z",
  refresh: "M21 3v6h-6M20.5 14a8.5 8.5 0 0 1-15.6 3.5",
} as const

export type IconName = keyof typeof ICON

export function Icon({
  name,
  size,
  style,
  className,
  strokeWidth,
}: {
  name: IconName
  size?: number
  style?: CSSProperties
  className?: string
  strokeWidth?: number
}) {
  const d = ICON[name]
  const s = size || 18
  return (
    <svg
      className={className}
      width={s}
      height={s}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth={strokeWidth || 1.7}
      strokeLinecap="round"
      strokeLinejoin="round"
      style={style}
    >
      {d
        .split("M")
        .filter(Boolean)
        .map((seg, i) => (
          <path key={i} d={"M" + seg} />
        ))}
    </svg>
  )
}

/* original isometric voxel logo — three rhombus faces, green tones */
export function Voxel({ className }: { className?: string }) {
  return (
    <svg className={"voxel " + (className || "")} viewBox="0 0 32 32" fill="none">
      <path d="M16 3 L29 10 L16 17 L3 10 Z" fill="var(--primary)" />
      <path d="M3 10 L16 17 L16 30 L3 23 Z" fill="oklch(0.45 0.13 150)" />
      <path d="M29 10 L16 17 L16 30 L29 23 Z" fill="oklch(0.54 0.155 150)" />
      <path d="M16 3 L29 10 L16 17 L3 10 Z" fill="oklch(1 0 0 / 0.12)" />
    </svg>
  )
}
