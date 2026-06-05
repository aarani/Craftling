import path from "path"
import tailwindcss from "@tailwindcss/vite"
import react from "@vitejs/plugin-react"
import { defineConfig } from "vite"

// https://vite.dev/config/
export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: {
      "@": path.resolve(__dirname, "./src"),
    },
  },
  server: {
    // Proxy API + health calls to the Go control plane so the browser stays
    // same-origin in dev (no CORS). Override the target with VITE_API_TARGET.
    proxy: {
      "/api": { target: process.env.VITE_API_TARGET || "http://localhost:8080", changeOrigin: true },
      "/healthz": { target: process.env.VITE_API_TARGET || "http://localhost:8080", changeOrigin: true },
    },
  },
})
