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

## P5 — World persistence (data disk → object/network storage)

- **Goal:** durable world data; precondition for safe rescheduling between hosts.
- **Why the shape:** on the squashfs+init image path (`internal/image`, `cmd/init`) the rootfs is mounted **read-only** and everything writable is **tmpfs (guest RAM)** — so a Minecraft world written under the workload's `WorkingDir` is **lost on stop**. (Only the legacy ext4 image path persisted, because the whole rootfs was a writable per-VM copy.) Durable persistence therefore requires a writable device that is *separate from the immutable image* and *separate from the ephemeral per-VM working dir*. We add that device, then layer backup/restore and cross-host migration on top. The build is split into three increments so each lands verifiable on its own.

### P5a — Per-server world data disk (writable device + guest overlay) ✅ landing now
- **Decision:** keep the read-only squashfs rootfs; attach a **second virtio-blk device** (`/dev/vdb`) backed by a per-server ext4 **world disk**, and have the in-VM init **overlay** it onto the workload's `WorkingDir`. `lowerdir` = the image's `WorkingDir` (read-only); `upperdir`/`workdir` = the world disk. The server jar and baked config show through from the image; every runtime write (world, logs, `server.properties` edits) lands on the world disk — which is exactly the unit a backup snapshots. This is the "overlay" option from the design discussion; the "rsync over vsock" alternative is folded into P5c as a *control* channel rather than a data pipe.
- **Steps:**
  - Host (`internal/agent/firecracker`): on `Provision` of a runspec VM with persistence enabled, create+format a per-server world disk under a `DataDir` keyed by `server_id` (decoupled from the per-VM `WorkDir` that `Deprovision` wipes), `mkfs.ext4` it once, and attach it as a non-root drive in `machine.configure`. The disk survives stop/start (same path re-attached on `Start`); `Deprovision` removes it (P5b snapshots first).
  - Contract (`internal/runspec`): add `Persist {Device, Mountpoint}` to `RunSpec`, published via MMDS like `Net`.
  - Guest (`cmd/init`): when `Persist` is set, mount `Device` (ext4) on a `/run` tmpfs scratch dir (the rootfs is read-only), then `mount -t overlay` with `lowerdir=WorkingDir` onto `WorkingDir`.
  - Config gate (`FC_WORLD_PERSIST`, `FC_DATA_DIR`, `FC_WORLD_DISK_MB`): off by default so MMDS-only hosts are unaffected. Requires `mkfs.ext4` on the host and `CONFIG_OVERLAY_FS` + `CONFIG_EXT4_FS` in the guest kernel.
- **New code:** `firecracker` world-disk create/attach + config; `runspec.PersistConfig`; `cmd/init/persist_linux.go`.
- **Verify:** non-KVM unit tests (world-disk path/sanitization, reuse-existing idempotency, config defaults/validation); KVM integration test — write a token under `WorkingDir` on first boot, `Stop`, `Start`, assert the token survived (proves it landed on the data disk, not tmpfs).

### P5b — World store: snapshot, restore, reschedule ✅ (DirStore; S3 follow-up)
- **Decision:** the store moves opaque bytes keyed by `server_id`; the agent owns the disk↔stream codec (gzip of the raw ext4 image — a mostly-zeroed disk compresses to almost nothing, so snapshots are cheap without the store knowing about filesystems). Lifecycle is anchored to how the reconciler actually calls the driver: **Provision restores** from the store (else formats fresh), **Stop snapshots** into it (the guest has powered off and synced, so the image is consistent), **Deprovision deletes** the blob (deprovision = server delete). The reschedule path is therefore *Stop on host A → Provision on host B*, both store-mediated — not Deprovision, which destroys.
- **Done:**
  - `internal/storage`: `WorldStore` interface (`Exists`/`Put`/`Get`/`Delete`, `ErrWorldNotFound`) + `DirStore`, a filesystem/NFS-mount backend (stdlib only, atomic temp+rename, key-sanitized). Point several agents at one shared mount and they see the same worlds — enough for cross-host reschedule without object storage.
  - `firecracker`: gzip snapshot/restore codec (`worldsnapshot.go`); `Config.WorldStore`; restore-on-Provision (`prepareWorldDisk`), snapshot-on-Stop, delete-on-Deprovision; wired through `internal/config` (`FC_WORLD_STORE_DIR`) + `cmd/agent`.
  - Verified: storage round-trip / containment / no-partial-on-error unit tests; codec round-trip + restore-vs-fresh prep tests; KVM e2e `TestKVMWorldStoreReschedule` — two runtimes sharing one store, world moves A→B only through the store.
- **S3 backend ✅:** `internal/storage/s3` — an S3-compatible `WorldStore` over minio-go (works against AWS S3, MinIO, Ceph, …), kept in its own package so the SDK stays out of the lightweight `internal/storage` import the driver pulls in. Static keys or the AWS credential chain (IAM role); `New` verifies the bucket at startup; objects keyed `<prefix><SafeKey>.world` (the keying helper is now shared with `DirStore`). Store construction is centralized in `internal/worldstore.FromConfig`, used by both binaries; selected via `FC_WORLD_STORE_S3_*` (S3 takes precedence over `FC_WORLD_STORE_DIR`). Verified by a MinIO-testcontainer e2e (`-tags e2e`) covering round-trip, replace, not-found→`ErrWorldNotFound`, idempotent delete, and missing-bucket startup failure — run green against a live MinIO.
- **World GC ✅:** `reaper.Worlds` (control plane, hourly) sweeps the store for snapshots no live server claims — orphans from a host that was permanently gone at delete time, or force-removed rows. It lists stored keys vs `GameServerRepository.ListActiveIDs` (matched through `SafeKey`) and deletes the difference; a failure to enumerate live servers deletes nothing (never reads a DB blip as "no servers"). `WorldStore` gained `List`; the control-plane binary now links the store SDK for GC (the driver still imports only the interface). The agent's delete-on-deprovision remains the happy-path cleanup; GC is the backstop.

P5 follow-ups (now done):
- **On-demand backup trigger ✅** — `POST /api/v1/servers/:id/snapshot` sets a `backup_requested` flag (migration `00003`); the reconciler honors it (snapshot a running server via `Provisioner.Snapshot` → agent, or treat a stopped server as already-durable), clears the flag, and stamps `last_backup_at`. This respects the sole-writer invariant: the API records intent, the reconciler performs the side effect. Failures leave the flag set for retry without flipping the server to `error`.
- **Async upload off the freeze window ✅** — a live snapshot now gzips the disk to a local temp *while frozen*, thaws immediately, then uploads the temp, so the freeze lasts only a local read+compress rather than a (possibly remote S3) upload.

### P5c — Consistent snapshots (vsock quiesce control channel) ✅ (mechanism + periodic; on-demand at agent boundary)
- **Why:** a Minecraft server writes region files continuously; a snapshot taken mid-write is torn. The agent quiesces the game (RCON `save-off` + `save-all flush`) and `fsfreeze`s the world-disk filesystem, snapshots, then resumes — with an **ack** the read-only MMDS path can't carry, so it rides a vsock control channel.
- **Done:**
  - Contract (`internal/runspec`): `QuiesceConfig{RCONAddress, RCONPassword}` + `VsockControlPort` + the line protocol constants (`PREPARE`/`RESUME`/`OK`/`ERR`).
  - Guest (`cmd/init`): an AF_VSOCK control server (`vsock_linux.go`) — on `PREPARE` it RCON-flushes (a minimal stdlib Source-RCON client, `rcon.go`) then `FIFREEZE`s the world-disk fs; on `RESUME` it `FITHAW`s + `save-on`. The freeze is held across the connection, so a dropped host always thaws (deferred).
  - Host (`firecracker`): `PutGuestVsock` per VM; `quiesce.go` drives the Firecracker UDS `CONNECT` handshake + `PREPARE → snapshot → RESUME` (thaw deferred so a snapshot failure never wedges the disk frozen); a **periodic sweep** (`SnapshotInterval`, `FC_SNAPSHOT_INTERVAL`) snapshots every running VM, bounding crash-loss to one interval. Knobs `FC_RCON_PORT`/`FC_RCON_PASSWORD`.
  - **On-demand**: at the agent boundary (`agent.Runtime.Snapshot` + `POST /vms/:id/snapshot` + `agent.Client.Snapshot`) and, end to end, the user-facing `backup_requested` flow above; the periodic sweep covers the autonomous half of "both".
  - Verified: RCON codec + fake-server unit tests; host vsock orchestration against a fake guest (PREPARE precedes RESUME; thaw runs even on snapshot failure); agent snapshot HTTP round-trip; reconciler-driven backup + ownership e2e; KVM e2e `TestKVMLiveSnapshot` — live-snapshot a *running* VM (freeze-only; busybox has no RCON) and restore it on a second runtime.

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
| P5 | — | world disk + overlay (`firecracker`, `runspec.Persist`, `cmd/init`); `internal/storage` (`WorldStore`, `DirStore`, `s3`); `internal/worldstore`; vsock quiesce; `reaper.Worlds` | per-server world disk (host file) + world snapshot (store blob); `game_servers.backup_requested/last_backup_at` (`00003`) |
| P6 | — | agent networking | `ports` (or host range) |
| P7 | — | metrics, health probes | `server_health` / cols |
| P8 | — | backoff, reschedule, leader election | `game_servers.attempts/next_attempt_at` |
| P9 | — | quota enforcement | `user_quotas` |
| P10 | — | agent auth, secrets | per-host agent creds |
