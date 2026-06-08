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

**P4 done:** real Firecracker microVMs on the agent. `internal/agent/firecracker` — a `Runtime` driver that boots each game server as a Firecracker microVM, driving Firecracker through the **in-repo generated REST client** (`internal/firecracker`) spoken over the per-VM API **Unix socket** (a custom `http.Client` dials the socket; the go-openapi client is otherwise unchanged), and managing the Firecracker process lifecycle directly. `Provision` stages a per-VM **writable copy** of the version's base rootfs, launches `firecracker --api-sock`, waits for the socket, configures machine (vCPU/mem from spec) + boot source (shared `vmlinux` + boot args) + root drive, then `InstanceStart`. `Stop` sends `SendCtrlAltDel` and force-kills after a grace period **but keeps the rootfs** (so the world survives a restart on this host — cross-host persistence is P5); `Start` re-boots a stopped VM from that same rootfs; `Deprovision` kills the process and removes the working dir; `Status` derives running/stopped/missing from process liveness. A per-version **image catalog** resolves `minecraft-<version>.ext4` (with a configurable default), so an unsupported version fails the provision rather than booting a wrong world. New `config.RuntimeFake`/`RuntimeFirecracker` selector + `FirecrackerConfig` (paths via `FC_*` env); `cmd/agent` picks the backend (`newRuntime`) — the agent HTTP seam, control-plane, and `RemoteProvisioner` are all unchanged because the driver satisfies the same `agent.Runtime` interface. The kernel + per-version rootfs images are provided out of band on the host for now (the build pipeline is deferred); `make test-kvm` runs the gated integration test. Networking is still the host advertise address (real per-VM networking is P6); jailer/seccomp hardening is P10. Unit tests (no KVM) cover config/artifact validation, version→image resolution + default fallback, invalid-spec/unknown-version rejection, the no-process idempotency edges (stop/deprovision unknown → nil, start unknown → `ErrVMNotFound`, status unknown → missing), and rootfs staging; a full provision→stop→start→deprovision lifecycle integration test is gated behind the `kvm` build tag (`make test-kvm`) and kept out of the default CI lane.

**P3 done:** control plane ↔ host agent split. `internal/agent` — `Runtime` interface + `FakeRuntime` (in-memory VM lifecycle), an HTTP `Server`/`NewRouter` exposing `POST /vms`, `POST /vms/:id/start|stop`, `DELETE /vms/:id`, `GET /vms/:id`, a `Client` the control plane uses to call an agent, and a `CPClient` the agent uses to register + heartbeat. `provisioner.RemoteProvisioner` implements `Provisioner` by resolving the assigned host's address from the in-memory inventory (`HostResolver`) and calling its agent — the reconciler's call *shape* is unchanged, it just became a network hop, so the control plane never touches KVM. New binary `cmd/agent` (FakeRuntime + register/heartbeat loop with re-register on CP-forgot-me) and `internal/config` `AgentConfig`. The control plane now wires `RemoteProvisioner` instead of `Fake`. Agent state strings mirror `provisioner.State` for 1:1 mapping. Per-VM observed status flows back via `Status` (the seam for P7 drift/health). e2e runs an in-process FakeRuntime agent and drives the full lifecycle across the real HTTP seam (server reaches `running`, the agent reports the VM running tagged with the server id, delete deprovisions it); unit tests cover the runtime, the server↔client round trip, and the RemoteProvisioner (lifecycle, unplaced, start-provisions-fresh).

**P2 done:** `internal/scheduler` — least-loaded placement over the in-memory fleet with **atomic capacity reservation** (`HostRepository.Reserve`/`Release` under the existing lock; `Reserve` is the race-safe commit point, the scheduler picks from a snapshot but only a host that still fits accepts). `game_servers.host_id` (migration `00002`, nullable, **no FK** — referential integrity is the scheduler's job). The reconciler places a `running`-desired, unassigned server before booting its VM; if nothing fits it marks the server `unschedulable` (a new status) and retries next tick; on delete it releases the host capacity. `host_id` persists across stop/start (the VM stays put) and is cleared only on delete. Create-time validation rejects a spec larger than any host's *total* capacity (`Scheduler.CanEverFit`; with no hosts yet it permits creation to wait). Identity reservations are reset to total on a control-plane restart (in-memory fleet) — a known limitation until a durable inventory lands. e2e covers placement (server reaches `running` with a `host_id`, host allocatable reduced) + oversize → `400`; scheduler unit tests cover spread, capacity/memory bounds, down-host exclusion, release, and concurrent reservation atomicity.

**P1 done:** `model.Host` + **in-memory** `HostRepository` (`internal/repository/host.go`, concurrency-safe map — no durable table yet); agent endpoints `POST /api/v1/agent/hosts/register` + `POST /api/v1/agent/hosts/:id/heartbeat` behind a placeholder `middleware.AgentAuth` seam; admin fleet view `GET /api/v1/admin/hosts`; host reaper (`reaper.Hosts`, 30s TTL / 10s sweep) marks stale hosts `down`, heartbeat recovers them to `ready`. **Identity is agent-owned**, not control-plane-assigned: register accepts an optional agent-supplied `id` (authoritative key on upsert), so a host keeps its id across a control-plane restart even though the fleet lives only in memory — this is why no `hosts` table is needed yet. e2e covers register → heartbeat → stale → `down` → recover, plus agent-supplied-id stability.

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

## P1 — Host fleet ✅

- **Goal:** model the pool of worker hosts.
- **Why:** scheduling and every compute action target a host; need an inventory + liveness first.
- **Steps:**
  - ~~`hosts` table~~ → **in-memory `HostRepository`** holding the same fields (`id, hostname, address, zone, cpus_total, memory_mb_total, cpus_allocatable, memory_mb_allocatable, status (ready|draining|down), agent_version, last_heartbeat_at, timestamps`). The fleet is reconstructable from live heartbeats, so no durable table is required at this phase; the repo's method set is DB-shaped so a Postgres store can slot in later unchanged.
  - `internal/repository/host.go` (`HostRepository`): upsert-register, heartbeat, get, list, list-ready (capacity-query seam for P2), mark-stale.
  - Agent-facing endpoints: `POST /api/v1/agent/hosts/register`, `POST /api/v1/agent/hosts/:id/heartbeat` (agent auth is a placeholder now; hardened in P10).
  - Host reaper (reuse `internal/reaper` pattern): mark hosts `down` when heartbeat goes stale; a later heartbeat recovers a host to `ready`.
  - **Identity (decision):** *agent-owned ids* — register accepts an optional agent-supplied `id`, the authoritative upsert key. A host keeps its id across a control-plane restart (the agent re-registers with the same id), so future `game_servers.host_id` references survive restarts without persisting the fleet. A durable `hosts` table is only needed later if we want declarative inventory (remembering a host that is `down` *and* silent).
  - **Comms model (decision):** *control-plane-authoritative hybrid* — agents push status up via heartbeat; the control plane enacts desired state by calling **down** to agents (P3).
- **New code:** `model.Host`, in-memory `HostRepository`, agent host handlers, `middleware.AgentAuth` placeholder, host reaper, admin `GET /api/v1/admin/hosts`.
- **Verify:** e2e — register host → heartbeat → stale → `down` → recover; agent-supplied-id stability. ✅

## P2 — Scheduler / placement ✅

- **Goal:** assign each unplaced server to a host with capacity.
- **Steps:**
  - ✅ Add `host_id` to `game_servers` (nullable; a plain id column, **not** a DB FK while the fleet is in-memory — referential integrity is the scheduler's job); add an `unschedulable` signal (status + `status_message`). Relies on P1's agent-owned ids staying stable across restarts.
  - ✅ `internal/scheduler`: pick a `ready` host with enough allocatable cpu/mem (least-loaded/first-fit); **reserve capacity atomically** — the in-memory fleet has no DB transaction, so the reservation commits under `HostRepository`'s lock (`Reserve`), and the scheduler treats a lost race as "try the next candidate".
  - ✅ Reconciler: if a `running`-desired server has no `host_id`, call the scheduler; if nothing fits, mark `unschedulable` and retry next tick. Capacity is released on delete.
  - ✅ Create-time validation: reject specs larger than any host can ever fit (`CanEverFit`), allowing creation when the fleet is still empty.
- **New code:** `internal/scheduler`; `host_id` column (migration `00002`); `HostRepository.Reserve`/`Release`.
- **Verify:** ✅ scheduler unit tests (spread, capacity/memory bounds, down-host exclusion, release, concurrent reservation) + e2e (placement reaches `running` with `host_id` and reduced host allocatable; oversize → `400`).

## P3 — Agent split (control plane ↔ host agent) ✅

- **Goal:** move VM execution off the control-plane process onto the host.
- **Why:** the control plane must not run KVM; today the reconciler calls `Provisioner` in-process.
- **Steps:**
  - ✅ New binary `cmd/agent` + `internal/agent` with a `Runtime` interface (`FakeRuntime` first) and a REST agent API: provision/start/stop/deprovision/status of local VMs (`agent.Server`/`NewRouter`).
  - ✅ `internal/provisioner`: `RemoteProvisioner` implements `Provisioner` by calling the assigned host's agent API (address resolved from the in-memory host inventory via `host_id`). The reconciler's call *shape* is unchanged — it just became a network hop.
  - ✅ Agent registers + heartbeats (P1, via `agent.CPClient`) and reports per-VM observed status through `Status`; the **drift→`game_servers.status`** reconcile loop is intentionally left to P7/P8 (the `Status` seam is in place).
- **New code:** `cmd/agent`, `internal/agent` (`Runtime`, `FakeRuntime`, `Server`, `Client`, `CPClient`), `provisioner.RemoteProvisioner`, `config.AgentConfig`.
- **Verify:** ✅ e2e runs the control plane + an in-process `FakeRuntime` agent and exercises the full lifecycle across the real HTTP seam; unit tests cover the runtime, server↔client round trip, and RemoteProvisioner.
- **Deferred:** agent↔control-plane auth (still the placeholder seam) and the deploy split land in P10; real per-VM status drift reconciliation in P7/P8.

## P4 — Firecracker runtime ✅

- **Goal:** real microVMs on the agent.
- **Steps:**
  - ✅ `internal/agent/firecracker`: `Runtime` driver — boot with `vmlinux` kernel + per-version rootfs, vCPU/mem from spec, manage the API socket, lifecycle (provision/start/stop/deprovision/status). Built on the **in-repo generated REST client** (`internal/firecracker`) over the per-VM Unix socket and direct Firecracker process management, rather than pulling in `firecracker-go-sdk`. Stop = `SendCtrlAltDel` + force-kill keeping the rootfs; start re-boots from it. Jailer/seccomp isolation is layered in P10.
  - ⏳ Minecraft rootfs image build pipeline (bootstrap → JRE + server jar by version → EULA accept → RCON enabled → init → pack ext4) is **deferred**; for now the per-version rootfs images and shared `vmlinux` kernel are provided out of band on the host (paths via `FC_KERNEL` / `FC_IMAGE_DIR`). The driver consumes them through the per-version image catalog.
- **New code:** ✅ `internal/agent/firecracker`; per-version rootfs catalog; `config.FirecrackerConfig` + runtime selector; `cmd/agent` backend wiring. (Image-build scripts deferred.)
- **Verify:** ✅ KVM-gated lifecycle integration test behind the `kvm` build tag (`make test-kvm`) on a `/dev/kvm` host, kept out of the default CI lane; non-KVM unit tests cover validation, image resolution, and idempotency edges. Manual: connect a Minecraft client.

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
| P4 | — | `internal/agent/firecracker` (image build deferred) | — |
| P5 | — | `internal/storage` | — |
| P6 | — | agent networking | `ports` (or host range) |
| P7 | — | metrics, health probes | `server_health` / cols |
| P8 | — | backoff, reschedule, leader election | `game_servers.attempts/next_attempt_at` |
| P9 | — | quota enforcement | `user_quotas` |
| P10 | — | agent auth, secrets | per-host agent creds |
