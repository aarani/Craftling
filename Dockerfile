# syntax=docker/dockerfile:1

# --- Build stage ---
FROM golang:1.26-alpine AS build

WORKDIR /src

# Cache dependencies first.
COPY go.mod go.sum ./
RUN go mod download

# Build the statically-linked server binary.
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/server ./cmd/server

# --- Runtime stage ---
FROM cgr.dev/chainguard/static:latest

WORKDIR /app
COPY --from=build /out/server /app/server

ENV PORT=8080 \
    APP_ENV=production

EXPOSE 8080
USER nonroot:nonroot

ENTRYPOINT ["/app/server"]
