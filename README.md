# craftling-go

A Go HTTP service built with [Gin](https://github.com/gin-gonic/gin).

The control plane for a multi-host, Firecracker-microVM Minecraft hosting
platform. See [PLAN.md](docs/PLAN.md) for the phased roadmap from here to production.

## Requirements

- Go 1.26+
- PostgreSQL 13+

## Getting started

```bash
cp .env.example .env   # set DATABASE_URL, JWT_SECRET, etc.

# Spin up Postgres (example via Docker):
docker run -d --name craftling-pg -e POSTGRES_PASSWORD=postgres \
  -e POSTGRES_DB=craftling -p 5432:5432 postgres:16-alpine

make run               # or: go run ./cmd/server
```

The server listens on `:8080` by default and applies the schema
(`internal/db/schema.sql`) automatically on startup.

## Configuration

| Variable       | Default                                                          | Description                      |
| -------------- | --------------------------------------------------------------- | -------------------------------- |
| `PORT`         | `8080`                                                          | HTTP listen port                 |
| `APP_ENV`      | `development`                                                   | `production` → release mode + JSON logs |
| `DATABASE_URL` | `postgres://postgres:postgres@localhost:5432/craftling?sslmode=disable` | Postgres connection string |
| `JWT_SECRET`   | `dev-secret-change-me`                                          | HMAC signing secret (**set in prod**) |
| `ACCESS_TTL`   | `15m`                                                          | Access-token (JWT) lifetime      |
| `REFRESH_TTL`  | `720h` (30d)                                                  | Refresh-token lifetime           |
| `ADMIN_EMAIL`    | _(unset)_                                                    | If set with `ADMIN_PASSWORD`, seeds/promotes this admin on startup |
| `ADMIN_PASSWORD` | _(unset)_                                                    | Admin bootstrap password         |

## Endpoints

| Method | Path                     | Auth   | Description                          |
| ------ | ------------------------ | ------ | ----------------------------------- |
| GET    | `/healthz`               | —      | Liveness probe                      |
| GET    | `/api/v1/ping`           | —      | Example endpoint                    |
| POST   | `/api/v1/auth/register`  | —      | Create user, returns a token pair   |
| POST   | `/api/v1/auth/login`     | —      | Verify credentials, returns a pair  |
| POST   | `/api/v1/auth/refresh`   | —      | Rotate refresh token, returns a pair |
| POST   | `/api/v1/auth/logout`    | —      | Revoke a refresh token (`204`)      |
| GET    | `/api/v1/me`             | Bearer | Current authenticated user          |
| POST   | `/api/v1/servers`        | Bearer | Create a game server                |
| GET    | `/api/v1/servers`        | Bearer | List your game servers              |
| GET    | `/api/v1/servers/:id`    | Bearer | Get one of your servers             |
| PATCH  | `/api/v1/servers/:id`    | Bearer | Rename, or set `desired_state` (running/stopped) |
| DELETE | `/api/v1/servers/:id`    | Bearer | Tear down a server (`202`)          |
| GET    | `/api/v1/admin/users`    | Admin  | List all users (role `admin` only)  |
| GET    | `/api/v1/admin/servers`  | Admin  | List all servers across all owners  |

Auth endpoints return a token pair:

```json
{
  "access_token": "<jwt>",
  "refresh_token": "<opaque>",
  "token_type": "Bearer",
  "expires_in": 900
}
```

The short-lived **access token** is sent as `Authorization: Bearer <access_token>`
on protected routes. When it expires, exchange the **refresh token** at
`/auth/refresh` for a new pair. Refresh tokens are **rotated** on every use
(the old one is revoked); replaying a rotated token is treated as theft and
revokes all of that user's tokens.

```bash
# Register (or login) to get a token pair
PAIR=$(curl -s -X POST localhost:8080/api/v1/auth/register \
  -H 'Content-Type: application/json' \
  -d '{"email":"alice@example.com","password":"hunter2pass"}')
ACCESS=$(echo "$PAIR" | jq -r .access_token)
REFRESH=$(echo "$PAIR" | jq -r .refresh_token)

# Call a protected route
curl localhost:8080/api/v1/me -H "Authorization: Bearer $ACCESS"

# Later: rotate for a fresh pair
curl -s -X POST localhost:8080/api/v1/auth/refresh \
  -H 'Content-Type: application/json' \
  -d "{\"refresh_token\":\"$REFRESH\"}"

# Log out (revoke the refresh token)
curl -s -X POST localhost:8080/api/v1/auth/logout \
  -H 'Content-Type: application/json' \
  -d "{\"refresh_token\":\"$REFRESH\"}"
```

### Roles

Each user has a single role (`user` by default, or `admin`). The role is
embedded in the access-token claims, and `RequireRole` middleware guards
admin routes — so authorization is checked without a database round-trip.

Because the role lives in the JWT, a role change takes effect on the user's
next token refresh (within `ACCESS_TTL`), not instantly.

Set `ADMIN_EMAIL` + `ADMIN_PASSWORD` to bootstrap an admin on startup: a
matching user is created if absent, or promoted to `admin` if it already
exists (its password is left untouched).

## Game servers & the reconciler

A game server separates **desired state** (`running` / `stopped` / `deleted`,
set by the API) from **observed status** (`pending` → `provisioning` →
`running` → `stopping` → `stopped` → `deleting`, plus `error`). A background
**reconciler** (`internal/reconciler`) ticks periodically, finds servers whose
status doesn't match their desired state, and drives them one step at a time
via a `Provisioner` backend.

The current backend is `provisioner.Fake`, which simulates VM provisioning
(assigns a synthetic `vm_id` and `host:25565`). Real microVM provisioning
(Firecracker / Cloud Hypervisor) drops in by implementing the same
`Provisioner` interface — the API and reconciler are unchanged.

```bash
# Create a server (desired_state defaults to running)
curl -s -X POST localhost:8080/api/v1/servers \
  -H "Authorization: Bearer $ACCESS" -H 'Content-Type: application/json' \
  -d '{"name":"survival","version":"1.20.4"}'

# Poll until the reconciler reports status "running" with host/port
curl -s localhost:8080/api/v1/servers/<id> -H "Authorization: Bearer $ACCESS"

# Stop / start
curl -s -X PATCH localhost:8080/api/v1/servers/<id> \
  -H "Authorization: Bearer $ACCESS" -H 'Content-Type: application/json' \
  -d '{"desired_state":"stopped"}'

# Tear down (reconciler deprovisions, then soft-deletes the row)
curl -s -X DELETE localhost:8080/api/v1/servers/<id> -H "Authorization: Bearer $ACCESS"
```

Deletes are **soft**: the reconciler deprovisions the VM and then sets
`status = deleted` with a `deleted_at` timestamp. The row is retained for
history/audit but hidden from every API read (and from reconciliation).

## Layout

```
cmd/server          entry point, DB connect/migrate + graceful shutdown
internal/config     environment configuration
internal/db         pgx pool + embedded schema/migration
internal/model      domain types (User)
internal/repository Postgres-backed data access
internal/auth       bcrypt password hashing + JWT issue/verify
internal/handler    routes and request handlers (incl. auth)
internal/middleware request ID, request logging, JWT auth guard
```

## Commands

```bash
make run       # run the server
make build     # build binary to ./bin/server
make test      # run unit tests
make test-e2e  # run end-to-end tests (requires Docker)
make tidy      # tidy go.mod
make fmt       # format code
```

## Testing

End-to-end tests live in `test/e2e/`. They are gated behind the `e2e` build
tag, so `make test` (plain `go test ./...`) never touches Docker. The e2e
suite uses [testcontainers](https://golang.testcontainers.org/) to start a
real Postgres, serves the actual router over HTTP, and drives the full
register → login → protected-route flow:

```bash
make test-e2e          # or: go test -tags e2e ./test/e2e/...
```
