# Craftling — Platform Roadmap

## Context

`craftling-go` today is a **control plane**: auth (JWT + rotating refresh tokens), roles, owner-scoped game-server CRUD, a desired-state/observed-status **reconciler**, and a `Provisioner` interface whose only implementation is `Fake` (returns `127.0.0.1:25565`, no-op teardown).

The goal is a **multi-host, Firecracker-microVM Minecraft hosting platform** with durable world storage. Decisions driving this roadmap: **Firecracker** microVMs · **multi-host fleet** from the start · **object/network storage** for world data · **full roadmap** to production.

This document is the phased plan to get there. Each milestone lists its goal, why it lands in that order, the concrete steps (anchored to existing code), the new components, and how it's verified.

## Guiding principles / invariants (hold across all phases)

- **Reconciliation is the core loop** — `desired_state` vs observed `status`; the reconciler is the *sole writer of compute side effects*.
- **`Provisioner` is the backend seam** — new compute backends slot in behind it without touching the API.
- **The control plane never touches KVM** — only the host agent does.
- **Owner-scoped, admin-visible, no-leak** — keep the `ownedOr404` pattern; admins get fleet-wide views.
- **Soft deletes / audit retained.**
- **Additive, idempotent schema changes** until P0 swaps in a real migration tool.

## Done so far (foundation)

Auth + refresh rotation + roles; `game_servers` CRUD + admin fleet-view; reconciler (2s) + token reaper; `Provisioner` interface with `Fake`; e2e suite (testcontainers) + CI; multi-stage Dockerfile.

**P0 done:** goose migrations (`internal/db/migrations`, applied on startup, clean on fresh + pre-existing DBs); `Provisioner` extended with `Start`/`Stop`/`Status` (stopped ≠ destroyed); `Mode` (`server`/`agent`) in `internal/config`.

---

## P0 — Foundations (no behavior change)

- **Goal:** prepare the codebase for distributed growth before adding it.
- **Why first:** schema and interfaces churn a lot next; do the boring groundwork once.
- **Steps:**
  - Adopt a migration tool (`golang-migrate` or `goose`). Convert `internal/db/schema.sql` into ordered migration files; replace the embedded-exec in `internal/db/db.go` `Migrate()`. Keep apply-on-startup.
  - Extend `provisioner.Provisioner` with `Start`/`Stop`/`Status` (so *stopped* ≠ *destroyed*); update `Fake` and `internal/reconciler` accordingly.
  - Add a `Mode`/`Role` to `internal/config` (`server` vs `agent`) ahead of the P3 binary split.
- **New code:** `internal/db/migrations/*.sql`; migration runner.
- **Verify:** existing e2e stays green; migrations apply cleanly on both fresh and pre-existing DBs.

## P1 — Host fleet

- **Goal:** model the pool of worker hosts.
- **Why:** scheduling and every compute action target a host; need an inventory + liveness first.
- **Steps:**
  - `hosts` table: `id, hostname, address, zone, cpus_total, memory_mb_total, cpus_allocatable, memory_mb_allocatable, status (ready|draining|down), agent_version, last_heartbeat_at, timestamps`.
  - `internal/repository/host.go` (`HostRepository`): upsert-register, heartbeat, list-ready, capacity queries, mark-down.
  - Agent-facing endpoints: `POST /api/v1/agent/hosts/register`, `POST /api/v1/agent/hosts/:id/heartbeat` (agent auth is a placeholder now; hardened in P10).
  - Host reaper (reuse `internal/reaper` pattern): mark hosts `down` when heartbeat goes stale.
  - **Comms model (decision):** *control-plane-authoritative hybrid* — agents push status up via heartbeat; the control plane enacts desired state by calling **down** to agents (P3).
- **New code:** `hosts` table, `HostRepository`, agent host handlers, host reaper.
- **Verify:** e2e — register host → heartbeat → stale → `down`.

## P2 — Scheduler / placement

- **Goal:** assign each unplaced server to a host with capacity.
- **Steps:**
  - Add `host_id` (nullable FK) to `game_servers`; add an `unschedulable` signal (status + `status_message`).
  - `internal/scheduler`: pick a `ready` host with enough allocatable cpu/mem (least-loaded/first-fit); **reserve capacity atomically** (transaction).
  - Reconciler: if a `running`-desired server has no `host_id`, call the scheduler; if nothing fits, mark `unschedulable` and retry next tick.
  - Create-time validation: reject specs larger than any host can ever fit.
- **New code:** `internal/scheduler`; `host_id` column; capacity-reservation logic.
- **Verify:** e2e — create N servers, assert placement spread; oversize request → `unschedulable`/`400`.

## P3 — Agent split (control plane ↔ host agent)

- **Goal:** move VM execution off the control-plane process onto the host.
- **Why:** the control plane must not run KVM; today the reconciler calls `Provisioner` in-process.
- **Steps:**
  - New binary `cmd/agent` + `internal/agent` with a `Runtime` interface (ship `FakeRuntime` first) and an agent API (gRPC or REST): provision/start/stop/deprovision/status of local VMs.
  - `internal/provisioner`: add `RemoteProvisioner` implementing `Provisioner` by calling the assigned host's agent API (address resolved from `hosts` via `host_id`). The reconciler's call *shape* is unchanged — it just becomes a network hop.
  - Agent registers + heartbeats (P1) and reports per-VM observed status, which reconciles into `game_servers.status`.
- **New code:** `cmd/agent`, `internal/agent` (`Runtime`, `FakeRuntime`, agent server), `provisioner.RemoteProvisioner`.
- **Verify:** e2e runs control plane + an in-process `FakeRuntime` agent; full lifecycle across the network seam.

## P4 — Firecracker runtime

- **Goal:** real microVMs on the agent.
- **Steps:**
  - `internal/agent/firecracker`: `Runtime` driver via `firecracker-go-sdk` + jailer — boot with `vmlinux` kernel + rootfs, vCPU/mem from spec, manage API socket, lifecycle (boot/pause/resume/stop/destroy).
  - Minecraft rootfs image: minimal Linux + JRE + server jar (by version) + EULA accept + RCON enabled + an init that pulls the world (P5) and launches the server. Versioned image-build pipeline (`build/firecracker`, `scripts/`, Make target).
  - Kernel image management.
- **New code:** `internal/agent/firecracker`; image-build scripts; per-version rootfs.
- **Verify:** KVM-gated integration test behind a new `kvm` build tag on a `/dev/kvm` host; manual: connect a Minecraft client. Keep this out of the default CI lane.

## P5 — World persistence (object/network storage)

- **Goal:** durable world data; precondition for safe rescheduling between hosts.
- **Steps:**
  - `internal/storage`: `WorldStore` interface (`Put`/`Get`/`Exists`) with an S3-compatible impl (and/or NFS mount); per-server world key.
  - Agent: pull world archive into the VM data disk before launch; on stop / periodically, RCON `save-all` + flush, archive, upload.
  - Reschedule: because the world lives in the store, deprovision on host A → provision on host B is safe.
- **New code:** `internal/storage` (`WorldStore`, S3 impl); snapshot logic in agent.
- **Verify:** stop on host A, force-start on host B, world intact; e2e with a MinIO testcontainer.

## P6 — Networking / player access

- **Goal:** players can actually connect.
- **Steps:**
  - Agent: per-VM tap device + IP; host NAT.
  - Port allocation: per-server port on the host's public IP (a `ports` table or host-local range); DNAT `host:port → vm:25565`; write `host`/`port` back to `game_servers` (columns already exist).
  - Future path: per-server hostname via a TCP/SNI proxy.
- **New code:** agent networking; port allocation.
- **Verify:** connect a client to `host_public_ip:allocated_port`.

## P7 — Observability / deep health

- **Goal:** know the *Minecraft process* is up, not just the VM.
- **Steps:**
  - Agent probes via RCON / Server List Ping: up?, player count, MOTD → report to control plane (`player_count`, `health`, `last_seen` on `game_servers` or a `server_health` table).
  - Prometheus `/metrics` on control plane + agent; log shipping hooks (zap already structured); surface `status_message`/health in API responses.
- **Verify:** e2e asserts health transitions; scrape `/metrics`.

## P8 — Reliability

- **Goal:** survive reconcile and host failures.
- **Steps:**
  - Reconciler retry/backoff: replace one-shot `status=error` with `attempts` + `next_attempt_at` (exponential backoff); `ListReconcilable` respects backoff.
  - Host failure: reaper marks host `down` → reschedule its servers (clear `host_id`) with **fencing** (generation token / ensure old VM gone) to avoid split-brain.
  - Draining: `draining` host → no new placement, migrate servers off.
  - Optional leader election (advisory lock/lease) if running multiple control-plane replicas.
- **Verify:** kill a host in test → servers rescheduled; error path retries with backoff.

## P9 — Quotas / resource controls

- **Goal:** bound per-user usage.
- **Steps:** `user_quotas` table (`max_servers`, `max_cpus`, `max_memory_mb`); enforce in Create/Update against current usage; admin endpoints to view/set.
- **Verify:** e2e — exceed quota → `403`.

## P10 — Hardening & ops

- **Goal:** production readiness.
- **Steps:**
  - Agent↔control-plane auth: per-host tokens or mTLS with rotation; lock down `/api/v1/agent/*`.
  - Secrets management: JWT/DB/object-storage creds from env/secret store; **fail fast** if `JWT_SECRET` is the default in production.
  - microVM isolation review (jailer, seccomp, cgroups, network policy).
  - Deploy split: control plane (static image, HA behind LB) vs agent (systemd on KVM hosts, needs `/dev/kvm`); config per role.
  - CI: keep the default Docker-only lane fast; add a self-hosted **KVM lane** for `kvm`-tagged tests; wire the image-build pipeline.
- **Verify:** security review; staged rollout.

---

## Dependency order

`P0 → P1 → P2 → P3 → P4` (compute path); `P5` after P3 (needs agent) and gates safe reschedule in `P8`; `P6` after P4; `P7`/`P9` after P3; `P8` after P2+P5; `P10` last, cross-cutting.

## New components at a glance

| Phase | Binaries | Packages | Tables/columns |
| --- | --- | --- | --- |
| P0 | — | `internal/db/migrations` | (migrations of existing) |
| P1 | — | `repository/host.go`, host reaper | `hosts` |
| P2 | — | `internal/scheduler` | `game_servers.host_id` |
| P3 | `cmd/agent` | `internal/agent`, `provisioner.RemoteProvisioner` | — |
| P4 | — | `internal/agent/firecracker`, image build | — |
| P5 | — | `internal/storage` | — |
| P6 | — | agent networking | `ports` (or host range) |
| P7 | — | metrics, health probes | `server_health` / cols |
| P8 | — | backoff, reschedule, leader election | `game_servers.attempts/next_attempt_at` |
| P9 | — | quota enforcement | `user_quotas` |
| P10 | — | agent auth, secrets | per-host agent creds |
