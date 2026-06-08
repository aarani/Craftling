# eBPF NAT dataplane (P6 networking)

> **Status:** design / not yet built. This is the spec to review before code lands.
>
> **Goal:** give each Firecracker microVM real connectivity using an eBPF
> datapath — no Linux bridge, and **no `iptables`/`nftables` rules** (the actual
> constraint: we refuse to manage userspace netfilter rule chains). We do the
> packet parse, rewrite, redirect, filtering, and observability in eBPF programs
> attached via TCX (kernel ≥ 6.6), and we **reuse the kernel's `nf_conntrack`
> table — driven directly from eBPF via the `bpf_ct_*` kfuncs** — for connection
> tracking, TCP state, and NAT source-port allocation. `nf_conntrack` is a kernel
> module, not a ruleset: it just needs to be loaded; we install zero rules.
>
> **Two flows in scope:**
> 1. **Egress** — VM → internet (Minecraft auth, mod downloads, etc.), with
>    destination filtering and per-flow observability.
> 2. **Ingress** — internet → VM: a host port (allocated per server, the P6
>    "random host port") DNAT'd to the in-VM service port (25565), with
>    source filtering and observability.

## 1. What we own vs. what we reuse

The constraint is "no iptables/nftables rules," not "no kernel code." So we split
the work where it's cheapest:

- **Reused (kernel `nf_conntrack` via `bpf_ct_*` kfuncs):** connection tracking,
  the full TCP state machine + timeouts, garbage collection of dead flows, and
  **NAT source-port allocation** (`bpf_ct_set_nat_info` reserves a unique manip
  tuple). These were the two hardest, most correctness-sensitive subsystems in
  the earlier "own everything" sketch — and they vanish. Cost: depend on the
  `nf_conntrack` module being loaded (no rules), kfuncs are ≥6.1 (we target ≥6.6).
- **Owned (eBPF):** packet parse, the actual header **rewrite** (we bypass the
  netfilter NAT hooks via `tc` redirect, so the kernel won't mangle for us — we
  read the allocated tuple off the `nf_conn` and rewrite the packet ourselves),
  `bpf_redirect`/`bpf_redirect_neigh` steering, egress/ingress filtering, and
  observability.

The sections below describe the owned datapath; conntrack appears as kfunc calls,
not as a hand-built map.

## 2. Topology & addressing

TAP-per-VM, **routed** (no bridge). Steering between a TAP and the uplink is done
with `bpf_redirect`/`bpf_redirect_neigh`, so the host kernel does **not** route
or forward these packets and `ip_forward` is **not** required.

```
                    ┌─────────────────────────── host ───────────────────────────┐
   internet ◀──────▶│  uplink NIC (HOST_IP)                                        │
                    │     │  ▲                                                     │
                    │  [tc ingress: nat_uplink]   (bpf_redirect_neigh out uplink)  │
                    │     ▼  │                                                     │
                    │   kernel nf_conntrack (via bpf_ct_* kfuncs) + DNAT/policy    │
                    │     ▲  │   maps (shared, pinned)                             │
                    │  [tc ingress: nat_tap]  ──── bpf_redirect(tapN, 0) ────┐     │
                    │     │                                                  ▼     │
                    │   tap0 (fc…)   tap1 (fc…)   …   tapN ───────────▶  Firecracker│
                    └────────────────────────────────────────────────────────────┘
                          │             │                │
                        VM 0          VM 1             VM N   (each: VM_IP/31, VM_MAC)
```

**Per-VM addressing** (new — today TAPs are link-local/MMDS-only):

- Each VM gets a private `VM_IP` and a deterministic `VM_MAC`. A `/31`
  point-to-point per TAP is sufficient (no host IP needed on the TAP in the
  redirect model — see §6 ARP).
- The VM's default gateway is a single shared virtual gateway IP (`GW_IP`, e.g.
  `169.254.0.1` or a chosen private addr). It is never assigned to a host
  interface; the eBPF ARP responder (or a static guest neighbor) resolves it.
- `VM_IP`/`GW_IP` are delivered to the guest via the **existing MMDS runspec**
  channel (`internal/runspec`), and applied by the in-VM init agent
  (`cmd/init/net_linux.go`) which today only sets the link-local MMDS address.

## 3. Programs and where they attach

A **single shared eBPF collection**, loaded once at agent start. This is a
structural change from the current per-TAP `tapfilter` (which loads one objects
set per TAP): the DNAT/policy maps must be shared across **all** TAPs and the
uplink, so we load once and attach the same programs to many ifindices. (The
conntrack table is the kernel's, shared by nature.)

| Program (`SEC`) | Attach point | Handles |
|---|---|---|
| `nat_tap` | TCX **ingress** on every `tapN` | VM-originated packets: new egress (SNAT) **and** replies to inbound (un-DNAT). ARP from guest. |
| `nat_uplink` | TCX **ingress** on the uplink, once | Internet-originated packets: replies to egress (un-SNAT) **and** new inbound to a published port (DNAT). |

Outbound, after rewrite, leaves via `bpf_redirect_neigh(uplink)`; inbound, after
rewrite, enters the VM via `bpf_redirect(tapN, 0)`. Neither needs an egress-hook
program. (An optional `obs_tap` on TAP egress can be added purely for
observability if we want post-rewrite visibility; not required.)

Programs may exceed the verifier's complexity budget as a monolith — split into
**tail calls**: `parse → classify → {snat, dnat, unsnat, undnat} → emit`.

## 4. Maps (all pinned to bpffs, shared)

| Map | Type | Key → Value | Purpose |
|---|---|---|---|
| `vm_config` | HASH | `tap_ifindex` / `vm_ip` → `{vm_ip, vm_mac, tap_ifindex}` | VM identity + where to redirect inbound |
| `dnat_rules` | HASH | `{proto, host_ip, host_port}` → `{vm_ip, vm_port, tap_ifindex, vm_mac}` | Published-port forwards (P6 host-port map) |
| `egress_policy` | LPM_TRIE | `{dst_cidr, port}` → verdict | Egress destination ACL (filtering) |
| `ingress_policy` | LPM_TRIE | `{src_cidr}` → verdict | Ingress source ACL (filtering) |
| `events` | RINGBUF | — | Flow observability to userspace |
| `stats` | PERCPU_HASH | `vm_ip` → counters | Per-VM bytes/pkts/drops/conns |

**No `ct` map and no `port_alloc` map.** Connection state lives in the kernel's
`nf_conntrack` table, reached via kfuncs (§5). That removes the silent-LRU-eviction
hazard, the `bpf_timer` expiry machinery, and the source-port bitmap entirely —
the kernel handles flow GC and unique-tuple allocation.

## 5. Connection tracking via kfuncs (the spine)

We never hand-build a CT map. Each packet looks up the kernel's `nf_conn`; new
flows are allocated, given a NAT binding, and inserted. The `nf_conn` carries the
original and reply tuples, so "where does this translate to" is read off the
entry — no two-keys-per-flow bookkeeping, no manual rewrite tables.

**kfunc vocabulary** (all release the ref with `bpf_ct_release` before return):

- `bpf_skb_ct_lookup(skb, tuple, sz, opts, sz)` → existing `nf_conn *` or NULL
- `bpf_skb_ct_alloc(...)` → new uncommitted `nf_conn *`
- `bpf_ct_set_nat_info(nfct, &addr, port, NF_NAT_MANIP_SRC|_DST)` → reserve a
  unique manip tuple (this is the source-port allocator)
- `bpf_ct_set_timeout` / `bpf_ct_set_status`, then `bpf_ct_insert_entry(nfct)`

The `bpf_ct_opts` carries `dir` so a lookup tells you whether the packet is in the
**original** or **reply** direction of its flow — that's how each hook decides
which way to rewrite. Because we steer with `tc` redirect (bypassing the netfilter
NAT hooks), the kernel does **not** mangle the packet; we read the relevant
tuple off the `nf_conn` and do the rewrite + checksum fixups (§7) ourselves.

### 5.1 Egress (VM → internet) — `nat_tap`

1. Parse; `egress_policy` lookup on `dst_ip/dport` → deny ⇒ drop + obs.
2. `bpf_skb_ct_lookup` on the VM-native tuple.
   - **Miss** ⇒ `bpf_skb_ct_alloc`; `bpf_ct_set_nat_info(NF_NAT_MANIP_SRC,
     HOST_IP, 0)` (port 0 ⇒ kernel picks a free source port); set timeout/status;
     `bpf_ct_insert_entry`. Re-read to get the allocated `aport`.
   - **Hit (original dir)** ⇒ read `aport` from the entry's reply tuple.
   - **Hit (reply dir)** ⇒ this is actually a reply to an *inbound* flow (§5.3);
     un-DNAT the source instead.
3. SNAT rewrite: `src = HOST_IP:aport`; fix IP + L4 checksums (§7).
4. `bpf_redirect_neigh(uplink_ifindex, NULL, 0, 0)` ⇒ `TC_ACT_REDIRECT`. Kernel
   does FIB + neighbor to the real gateway, fills L2.
5. `bpf_ct_release`; emit obs; bump `stats`.

### 5.2 Reply to egress (internet → VM) — `nat_uplink`

1. Parse. `bpf_skb_ct_lookup` on the packet tuple.
   - **Hit (reply dir of a SNAT flow)** ⇒ un-SNAT: `dst = VM_IP:sport` (from the
     entry's original tuple); fix checksums; L2 `dst=VM_MAC` (from `vm_config`);
     `bpf_redirect(tap_ifindex, 0)`; release; obs.
   - **Miss** ⇒ fall through to inbound DNAT (§5.3).

### 5.3 Inbound (new, internet → published port) — `nat_uplink`

After a `ct` miss in §5.2:

1. `dnat_rules` lookup `{proto, HOST_IP, dport}`.
   - **Miss** ⇒ `TC_ACT_OK` (let the host's own stack handle it — host services
     keep working).
   - **Hit** ⇒ `ingress_policy` lookup on `src_ip` → deny ⇒ drop + obs.
2. `bpf_skb_ct_alloc`; `bpf_ct_set_nat_info(NF_NAT_MANIP_DST, VM_IP, vm_port)`;
   timeout/status; `bpf_ct_insert_entry`.
3. DNAT rewrite: `dst = VM_IP:vm_port`; fix checksums; L2 `dst=VM_MAC`;
   `bpf_redirect(tap_ifindex, 0)`; release; obs.

### 5.4 Reply to inbound (VM → internet) — `nat_tap`

Same hook as egress. The `bpf_skb_ct_lookup` returns the §5.3 flow in the **reply
direction** (`opts.dir == reply`) ⇒ un-DNAT the source: `src = HOST_IP:host_port`
(from the original tuple); checksums; `bpf_redirect_neigh(uplink)`; release. So
`nat_tap` disambiguates new-egress vs reply-to-inbound purely from the lookup's
direction flag — no extra state of our own.

### 5.5 Expiry, GC, port reclamation — the kernel's job

Timeouts, the TCP state machine, dead-flow garbage collection, and freeing the
allocated source port all happen inside `nf_conntrack`. We set an initial timeout
on insert and otherwise do nothing: no `bpf_timer`, no bitmap, no Go sweeper. Tune
via the standard `nf_conntrack` sysctls (e.g. `nf_conntrack_tcp_timeout_*`) if the
defaults don't suit; that's configuration, not rules.

## 6. ARP / gateway resolution

The VM ARPs for `GW_IP`. With no host IP on the TAP, nothing answers. Two
options:

- **eBPF ARP responder** (self-contained): `nat_tap` detects ARP-request for
  `GW_IP` and crafts an ARP reply (`GW_IP` is-at `tap_mac`) back out the TAP.
- **Static guest neighbor** (simplest): `cmd/init` installs a permanent ARP entry
  `GW_IP → fixed gateway MAC`, so the guest never ARPs. Recommended to start; add
  the eBPF responder later if we want zero guest assumptions.

Inbound packets to the VM are addressed by us (`dst=VM_MAC`), so no inbound ARP
concern.

## 7. Checksums

- IPv4 header: `bpf_l3_csum_replace` over changed addr fields.
- TCP/UDP: `bpf_l4_csum_replace` with `BPF_F_PSEUDO_HDR` for the IP-address delta
  and a plain replace for the port delta; use `bpf_csum_diff` for multi-word
  deltas. Handle UDP zero-checksum.
- **virtio/TAP `CHECKSUM_PARTIAL` gotcha:** packets off the guest may carry
  partial checksums (`skb->ip_summed`). Validate behavior against the real
  virtio-net path early — this is the most common source of silently-corrupt NAT.

## 8. Filtering & observability

- **Egress ACL:** `egress_policy` LPM on `dst_ip/dport`, evaluated at `nat_tap`
  before SNAT (sees true public destination).
- **Ingress ACL:** `ingress_policy` LPM on `src_ip`, at `nat_uplink` before DNAT
  CT creation (sees true remote source — DNAT only rewrites dst).
- **Obs:** ringbuf event per translation carrying pre+post tuples, direction,
  verdict, length. Userspace (Go) drains and correlates: it knows the
  `host_port → VM:vm_port` mapping and `vm_ip → server`, so events are annotated
  with server identity and the public-facing port without observing the uplink
  separately. Per-VM counters in `stats`.

## 9. Go-side responsibilities

- **IPAM:** allocate `VM_IP`, deterministic `VM_MAC`, and the published
  `host_port`; record the `host_port → VM:25565` mapping (this is P6's per-server
  host-port allocation; surface it where `AdvertiseHost` is reported today).
- **Map population:** `vm_config`, `dnat_rules`, policy maps on provision; remove
  on deprovision.
- **Attach:** load the shared collection once at `Runtime.New`; attach
  `nat_uplink` to the uplink (new `Config.UplinkDevice`); attach `nat_tap` to each
  TAP's ifindex in `createTAP`, detach in `deleteTAP` (mirrors the current
  `tapfilter` wiring).
- **Guest config:** publish `VM_IP`/`GW_IP`/route via MMDS runspec; extend
  `cmd/init/net_linux.go` to apply them (+ static gateway neighbor per §6).
- **Obs:** drain `events`, export metrics. (No CT sweeper — the kernel GCs flows.)
- **Preflight:** ensure `nf_conntrack` is loaded (`modprobe nf_conntrack`) at agent
  start and fail fast with a clear error if the kfuncs aren't available.

## 10. Edge cases & risks (call out before building)

- **IP fragmentation:** L4 header only in the first fragment; NAT of later
  fragments needs reassembly or a frag table. Start by **dropping non-first
  fragments** (+ obs) and revisit.
- **ICMP:** echo needs ID-based NAT; ICMP errors embed the original packet whose
  inner tuple must also be rewritten. Phase 2 — start with TCP/UDP only.
- **TCP state & port allocation:** handled by `nf_conntrack` (the reason we
  reuse it). Nothing to build; just confirm the conntrack TCP timeouts suit us.
- **`nf_conntrack` must be enabled/loaded:** the kfuncs need the module present
  and conntrack active in the netns. This is a *module + sysctls* dependency, not
  iptables/nftables rules — consistent with the "no rules" constraint. Preflight
  and fail fast (§9).
- **We still rewrite the packet ourselves:** bypassing the netfilter NAT hooks via
  `tc` redirect means the kernel won't mangle; the kfuncs give us the tuple +
  state, the eBPF code applies it. Validate the read-tuple-then-rewrite flow in
  the Phase-1 spike.
- **kfunc availability:** `bpf_skb_ct_lookup`/`bpf_ct_release` since 5.19; the
  alloc/insert/`set_nat_info` family since 6.1 — fine on the ≥6.6 target, but it
  pins the minimum kernel.
- **CT lookup/insert races:** two packets of one new flow on different CPUs — the
  loser's `bpf_ct_insert_entry` fails; re-lookup and use the winner's entry.
- **Checksum offload (virtio):** §7 — verify first.
- **Host's own traffic:** `nat_uplink` must `TC_ACT_OK` on CT+DNAT miss so host
  services and SSH keep working.
- **Verifier limits:** tail-call split; bounded loops everywhere.
- **Capabilities/kernel:** needs `CAP_BPF`+`CAP_NET_ADMIN` (+ `CAP_SYS_ADMIN` for
  pinning), BTF (`CONFIG_DEBUG_INFO_BTF=y`), `CONFIG_NF_CONNTRACK`, kernel ≥ 6.6.

## 11. Build phases

0. **Addressing + guest config + gateway ARP** — VM gets an IP, default route,
   reaches the gateway. (Prerequisite; nothing routes without it.)
1. **kfunc conntrack spike + egress SNAT + reply** — prove the
   lookup/alloc/`set_nat_info`/insert → read-tuple → rewrite + checksum flow on
   the real virtio path; VM reaches the internet. No filtering.
2. **Inbound DNAT + VM-reply un-DNAT** — published host-port → VM:25565 works.
3. **Filtering + observability** — egress/ingress ACLs, ringbuf, stats.
4. **Hardening** — ICMP, fragmentation, conntrack-timeout tuning, checksum-offload
   validation.

Each phase is independently testable end-to-end (curl from guest for 1; external
client → host_port for 2). Flow expiry/GC needs no phase — it's the kernel's.

## 12. Relationship to existing code

- The current single-port `tapfilter` (`internal/agent/firecracker/bpf/tapfilter.c`
  + `tapfilter_linux.go`) is the **filter/obs seed**, but its per-TAP load model
  is replaced by the single shared collection of §3. The C grows into `nat.c`
  with tail-called programs; the Go loader moves to a once-at-startup attach with
  per-VM map writes.
- Lifecycle hooks are the same ones already wired: `createTAP`/`deleteTAP`
  (`tap_linux.go`), `Runtime.Provision`/`Deprovision` (`runtime.go`), and the
  MMDS runspec publish (`mmds.go`).
- `Config` gains `UplinkDevice` (and gateway/subnet knobs); `AdvertiseHost`'s
  stand-in is replaced by the real `HOST_IP:host_port` from §9 IPAM.
