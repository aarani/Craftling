# syntax=docker/dockerfile:1

# --- Build stage: compile the Vite app to static assets ---
FROM node:22-alpine AS build

WORKDIR /app

# Install dependencies first for better layer caching.
COPY package.json package-lock.json ./
RUN npm ci

# Build (tsc -b && vite build → /app/dist). Needs devDependencies, which
# `npm ci` installs by default.
COPY . .
RUN npm run build

# --- Runtime stage: serve the static build with nginx ---
FROM nginx:1.27-alpine

# SPA + API-proxy config. The base image's entrypoint expands ${API_UPSTREAM}
# via envsubst before starting nginx. Override at runtime to point at the
# control plane (e.g. -e API_UPSTREAM=http://server:8080).
ENV API_UPSTREAM=http://server:8080
COPY nginx.conf /etc/nginx/templates/default.conf.template

COPY --from=build /app/dist /usr/share/nginx/html

EXPOSE 80

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s \
    CMD wget -qO- http://localhost/ >/dev/null 2>&1 || exit 1
