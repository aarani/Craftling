# eBPF NAT dataplane (P6 networking)

> **Status:** design / not yet built. This is the spec to review before code lands.
>
> **Goal:** give each Firecracker microVM real connectivity using a pure-eBPF
> datapath — no Linux bridge, no `nftables`/`iptables`, no kernel conntrack. We
> own connection tracking, SNAT (masquerade), and DNAT (port-forward) in eBPF,
> attached via TCX (kernel ≥ 6.6). Filtering and observability are part of the
> same programs.
>
> **Two flows in scope:**
> 1. **Egress** — VM → internet (Minecraft auth, mod downloads, etc.), with
>    destination filtering and per-flow observability.
> 2. **Ingress** — internet → VM: a host port (allocated per server, the P6
>    "random host port") DNAT'd to the in-VM service port (25565), with
>    source filtering and observability.

## 1. Why full-eBPF here (and what it costs)

The hybrid alternative (eBPF filter + `nftables` NAT) is less code, but we chose
to own the dataplane. The cost is real and concentrated in **connection
tracking**: source-port allocation, forward/reverse 5-tuple rewrite, entry
expiry + port reclamation, and a TCP state subset. The rest (parse, checksum
fixups, redirect) is mechanical. The sections below treat conntrack as the spine
everything else hangs off.

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
                    │   conntrack / SNAT / DNAT maps (shared, pinned)              │
                    │     ▲  │                                                     │
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
set per TAP): conntrack/port/policy state must be shared across **all** TAPs and
the uplink, so we load once and attach the same programs to many ifindices.

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
| `ct` | HASH (+ `bpf_timer` per elem) | `ct_key` → `ct_entry` | Connection tracking (two entries/flow, §5) |
| `port_alloc` | ARRAY (bitmap) | word index → 64-bit word | SNAT source-port pool per (proto, host_ip) |
| `egress_policy` | LPM_TRIE | `{dst_cidr, port}` → verdict | Egress destination ACL (filtering) |
| `ingress_policy` | LPM_TRIE | `{src_cidr}` → verdict | Ingress source ACL (filtering) |
| `events` | RINGBUF | — | Flow observability to userspace |
| `stats` | PERCPU_HASH | `vm_ip` → counters | Per-VM bytes/pkts/drops/conns |

`HASH` (not `LRU_HASH`) for `ct`: LRU evicts silently with no callback, which
would **leak allocated source ports**. We instead expire entries with
`bpf_timer` (§5.4), which gives us a callback to free the port and delete the
sibling entry.

## 5. Connection tracking (the spine)

### 5.1 Entry model — two keys per flow

Each connection inserts **two** `ct` entries so a packet from either direction
finds it directly:

- **Original key** = the tuple as first seen (egress: `VM_IP:sport → DST:dport`;
  inbound: `REMOTE:sport → HOST_IP:host_port`).
- **Reply key** = the tuple of return packets *as they arrive* (egress reply:
  `DST:dport → HOST_IP:aport`; inbound reply from VM: `VM_IP:vm_port → REMOTE:sport`).

Each entry carries the **rewrite to apply** to packets matching that key, plus
the **sibling key** (so refresh/expiry updates both) and shared-ish state
(refreshed on both). Value sketch:

```c
struct ct_entry {
    __u32 new_saddr, new_daddr;   // rewrite targets (0 = leave)
    __u16 new_sport, new_dport;
    struct ct_key sibling;        // the other direction's key
    __u64 last_seen_ns;
    __u32 stats_idx;              // vm_ip for counters
    __u8  proto, tcp_state, flags;
    struct bpf_timer timer;
};
```

### 5.2 Egress (new outbound) — `nat_tap`

1. Parse; `egress_policy` lookup on `dst_ip/dport` → deny ⇒ drop + obs.
2. `ct` lookup on `{VM_IP:sport, DST:dport}`.
   - **Hit** ⇒ reuse the entry's `aport`; refresh timer.
   - **Miss** ⇒ allocate `aport` from `port_alloc` (§5.3); insert original key
     `{VM_IP:sport,DST:dport}`→rewrite(src→`HOST_IP:aport`) and reply key
     `{DST:dport,HOST_IP:aport}`→rewrite(dst→`VM_IP:sport`); arm timer.
3. SNAT rewrite: `src = HOST_IP:aport`; fix IP + L4 checksums (§7).
4. `bpf_redirect_neigh(uplink_ifindex, NULL, 0, 0)` ⇒ `TC_ACT_REDIRECT`. Kernel
   does FIB + neighbor to the real gateway, fills L2.
5. Emit obs; bump `stats`.

### 5.3 Reply to egress — `nat_uplink`

1. Parse. `ct` lookup on `{src=DST:dport, dst=HOST_IP:aport}`.
   - **Hit** ⇒ un-SNAT: `dst = VM_IP:sport`; fix checksums; set L2 `dst=VM_MAC`;
     `bpf_redirect(tap_ifindex, 0)`; refresh timer; obs.
   - **Miss** ⇒ fall through to inbound DNAT (§5.5).

### 5.4 Inbound (new) — `nat_uplink`

After a `ct` miss in §5.3:

1. `dnat_rules` lookup `{proto, HOST_IP, dport}`.
   - **Miss** ⇒ `TC_ACT_OK` (let the host's own stack handle it — host services
     keep working).
   - **Hit** ⇒ `ingress_policy` lookup on `src_ip` → deny ⇒ drop + obs.
2. Insert CT: original key `{REMOTE:sport, HOST_IP:host_port}`→rewrite(dst→
   `VM_IP:vm_port`) and reply key `{VM_IP:vm_port, REMOTE:sport}`→rewrite(src→
   `HOST_IP:host_port`); arm timer.
3. DNAT rewrite: `dst = VM_IP:vm_port`; fix checksums; L2 `dst=VM_MAC`;
   `bpf_redirect(tap_ifindex, 0)`; obs.

### 5.5 Reply to inbound (VM → remote) — `nat_tap`

Same hook as egress. The `ct` lookup on `{VM_IP:vm_port, REMOTE:sport}` hits the
reply key from §5.4 ⇒ un-DNAT source: `src = HOST_IP:host_port`; checksums;
`bpf_redirect_neigh(uplink)`. So `nat_tap` disambiguates new-egress vs
reply-to-inbound purely by whether a CT reply-key exists.

### 5.6 Expiry & port reclamation (`bpf_timer`)

Each CT entry arms a `bpf_timer`. Refresh re-arms it (UDP ~30s, TCP-established
~120s, TCP-SYN ~20s; shorten to ~10s on FIN/RST). On fire, the callback frees the
`port_alloc` bit (egress flows only) and deletes both this entry and its
`sibling`. This is the modern (≥5.15) replacement for a userspace GC; a periodic
Go sweeper is the fallback if `bpf_timer` proves fiddly.

### 5.7 Source-port allocation

Bitmap in `port_alloc` (8 KiB = 65536 bits per proto/host-IP). Lock-free claim:
scan from a per-CPU cursor, `__sync_fetch_and_or` the candidate bit, succeed if
the old bit was 0; bounded to N attempts (verifier needs a bounded loop) then
fail the flow. Allocation is global per (proto, HOST_IP) ⇒ **symmetric NAT**,
~64k concurrent flows/proto. Freed by the expiry callback.

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
- **Obs:** drain `events`, export metrics; optional CT sweeper fallback.

## 10. Edge cases & risks (call out before building)

- **IP fragmentation:** L4 header only in the first fragment; NAT of later
  fragments needs reassembly or a frag table. Start by **dropping non-first
  fragments** (+ obs) and revisit.
- **ICMP:** echo needs ID-based NAT; ICMP errors embed the original packet whose
  inner tuple must also be rewritten. Phase 2 — start with TCP/UDP only.
- **TCP state:** start with a coarse state subset (SYN / established / FIN-RST
  timeouts); full RFC tracking later. Out-of-state packets get the default
  timeout, not rejection, initially.
- **Port exhaustion:** ~64k/proto/host-IP; emit a stat and drop+log on
  exhaustion rather than silently misbehaving.
- **CT insert races:** two packets of one new flow on different CPUs — insert
  with `BPF_NOEXIST`, on `-EEXIST` re-lookup and reuse.
- **Checksum offload (virtio):** §7 — verify first.
- **Host's own traffic:** `nat_uplink` must `TC_ACT_OK` on CT+DNAT miss so host
  services and SSH keep working.
- **Verifier limits:** tail-call split; bounded loops everywhere.
- **Capabilities/kernel:** needs `CAP_BPF`+`CAP_NET_ADMIN` (+ `CAP_SYS_ADMIN` for
  pinning), BTF (`CONFIG_DEBUG_INFO_BTF=y`), kernel ≥ 6.6.

## 11. Build phases

0. **Addressing + guest config + gateway ARP** — VM gets an IP, default route,
   reaches the gateway. (Prerequisite; nothing routes without it.)
1. **Egress conntrack + SNAT + reply** — VM reaches the internet. No filtering.
2. **Inbound DNAT + VM-reply un-DNAT** — published host-port → VM:25565 works.
3. **Filtering + observability** — egress/ingress ACLs, ringbuf, stats.
4. **Expiry (`bpf_timer`) + port reclamation + TCP state subset.**
5. **Hardening** — ICMP, fragmentation, exhaustion handling, checksum-offload
   validation.

Each phase is independently testable end-to-end (curl from guest for 1; external
client → host_port for 2).

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
