//go:build ignore

// nat is the P6 eBPF NAT dataplane: one shared collection attached to every
// microVM TAP (nat_tap, TCX ingress) and once to the host uplink (nat_uplink,
// TCX ingress). It gives each Firecracker VM real connectivity with NO Linux
// bridge and NO iptables/nftables rules — packet parse, header rewrite,
// redirect, filtering and observability are owned here, while connection
// tracking, the TCP state machine, flow GC and NAT source-port allocation are
// delegated to the kernel's nf_conntrack via the bpf_ct_* kfuncs.
//
// See docs/ebpf-nat-dataplane.md for the design. Flows in scope: egress (VM ->
// internet, SNAT to HOST_IP) and its reply (un-SNAT), and inbound (internet ->
// a published host port, DNAT to VM:vm_port) and its reply (un-DNAT). TCP/UDP
// over IPv4 only; ICMP and fragmented packets are out of scope (dropped with an
// observability event for non-first fragments).
//
// REQUIRES: kernel >= 6.6, CONFIG_NF_CONNTRACK + CONFIG_NF_NAT, BTF, and a
// generated vmlinux.h in this directory (see gen.go). The bpf_ct_* alloc/insert
// /set_nat_info kfuncs are >= 6.1. This program reuses nf_conntrack as a *module*
// (it must be loaded) and installs zero rules — consistent with the "no rules"
// constraint.

#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_endian.h>
#include <bpf/bpf_core_read.h>

char __license[] SEC("license") = "GPL";

// ---- Constants not present in BTF (they are #defines, not types) ----------

#define ETH_P_IP 0x0800
#define ETH_P_ARP 0x0806
#define ETH_HLEN 14
#define ETH_ALEN 6

#define IPPROTO_TCP 6
#define IPPROTO_UDP 17

#define TC_ACT_OK 0
#define TC_ACT_SHOT 2
#define TC_ACT_REDIRECT 7

#define IP_MF 0x2000     // "more fragments" flag (host order after ntohs)
#define IP_OFFMASK 0x1fff // fragment offset mask

#define ARPHRD_ETHER 1
#define ARPOP_REQUEST 1
#define ARPOP_REPLY 2

#define BPF_F_CURRENT_NETNS (-1L)

// Flag/helper macros live in uapi <linux/bpf.h>, not in BTF, so vmlinux.h does
// not provide them — define the ones we use.
#define BPF_F_INGRESS 1
#define BPF_F_PSEUDO_HDR 0x10
#define BPF_NOEXIST 1
#ifndef offsetof
#define offsetof(type, member) __builtin_offsetof(type, member)
#endif
#ifndef NULL
#define NULL ((void *)0)
#endif

// nf_conntrack status bits we set on insert (subset of enum ip_conntrack_status).
#define IPS_CONFIRMED 0x8
#define IPS_SEEN_REPLY 0x2
#define IPS_ASSURED 0x4

// Default conntrack timeout (seconds) for a freshly inserted flow; nf_conntrack
// supersedes it as the TCP state machine advances.
#define CT_TIMEOUT 120

// Direction of the looked-up flow, matching bpf_ct_opts.dir / IP_CT_DIR_*.
#define CT_DIR_ORIGINAL 0
#define CT_DIR_REPLY 1

// Observability dir values (struct nat_event.dir).
#define EV_EGRESS 0        // VM -> internet (new/established, SNAT)
#define EV_EGRESS_REPLY 1  // internet -> VM (reply to egress, un-SNAT)
#define EV_INBOUND 2       // internet -> published port (new, DNAT)
#define EV_INBOUND_REPLY 3 // VM -> internet (reply to inbound, un-DNAT)
#define EV_DENY 4          // dropped by policy / parse

// vmlinux.h only forward-declares enum nf_nat_manip_type (no enumerators) and
// omits struct bpf_ct_opts entirely — BTF dedup drops both because nothing in
// the kernel's tracked types fully references them — so define them here.
// Completing the forward-declared enum is legal C. The bpf_ct_opts layout MUST
// match the running kernel's (the kfuncs reject a mismatched opts__sz): this is
// the >= 6.5 form with the ct_zone fields, NF_BPF_CT_OPTS_SZ == 16. Matches the
// kernel >= 6.6 requirement documented above.
enum nf_nat_manip_type {
	NF_NAT_MANIP_SRC,
	NF_NAT_MANIP_DST,
};

struct bpf_ct_opts {
	s32 netns_id;
	s32 error;
	u8 l4proto;
	u8 dir;
	u16 ct_zone_id;
	u8 ct_zone_dir;
	u8 reserved[3];
};

// ---- conntrack kfuncs ------------------------------------------------------
// Declared as kernel symbols; the verifier resolves them against BTF at load.

struct nf_conn___init;

extern struct nf_conn *bpf_skb_ct_lookup(struct __sk_buff *skb_ctx,
					 struct bpf_sock_tuple *bpf_tuple,
					 __u32 tuple__sz,
					 struct bpf_ct_opts *opts,
					 __u32 opts__sz) __ksym;
extern struct nf_conn___init *bpf_skb_ct_alloc(struct __sk_buff *skb_ctx,
					       struct bpf_sock_tuple *bpf_tuple,
					       __u32 tuple__sz,
					       struct bpf_ct_opts *opts,
					       __u32 opts__sz) __ksym;
extern struct nf_conn *bpf_ct_insert_entry(struct nf_conn___init *nfct) __ksym;
extern void bpf_ct_release(struct nf_conn *nf_conn) __ksym;
extern void bpf_ct_set_timeout(struct nf_conn___init *nfct, __u32 timeout) __ksym;
extern int bpf_ct_set_status(const struct nf_conn___init *nfct, __u32 status) __ksym;
extern int bpf_ct_set_nat_info(struct nf_conn___init *nfct,
			       union nf_inet_addr *addr, int port,
			       enum nf_nat_manip_type manip) __ksym;

// ---- Maps (pinned, shared across all TAPs + the uplink from userspace) -----

// global_config is one entry (key 0) written at startup: the uplink identity
// and the gateway, plus default ACL verdicts.
struct global_config {
	__u32 host_ip;        // network order; SNAT source address
	__u32 uplink_ifindex; // where egress redirects to
	__u32 gw_ip;          // network order; the guest's virtual gateway
	__u8 gw_mac[ETH_ALEN];
	__u8 default_egress_allow;  // 1 => allow flows with no egress_policy match
	__u8 default_ingress_allow; // 1 => allow flows with no ingress_policy match
};

struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__uint(max_entries, 1);
	__type(key, __u32);
	__type(value, struct global_config);
} global_config_map SEC(".maps");

// vm_entry identifies a VM by its private address: where to redirect inbound
// frames (tap_ifindex) and how to address them (vm_mac).
struct vm_entry {
	__u32 vm_ip;         // network order
	__u32 tap_ifindex;
	__u8 vm_mac[ETH_ALEN];
	__u8 _pad[2];
};

struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 4096);
	__type(key, __u32); // vm_ip, network order
	__type(value, struct vm_entry);
} vm_config SEC(".maps");

// dnat_key/dnat_val: published-port forwards (the P6 host-port map).
struct dnat_key {
	__u32 host_ip;   // network order
	__u16 host_port; // network order
	__u8 proto;
	__u8 _pad;
};
struct dnat_val {
	__u32 vm_ip;        // network order
	__u32 tap_ifindex;
	__u16 vm_port;      // network order
	__u8 vm_mac[ETH_ALEN];
};

struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 4096);
	__type(key, struct dnat_key);
	__type(value, struct dnat_val);
} dnat_rules SEC(".maps");

// Policy tries: LPM over an IPv4 prefix. verdict: 1 = allow, 0 = deny. port == 0
// matches any L4 port; otherwise only that destination port (host order). A
// lookup miss falls back to global_config.default_*_allow.
struct policy_key {
	__u32 prefixlen; // LPM: bits of addr that are significant
	__u32 addr;      // network order
};
struct policy_val {
	__u8 verdict;
	__u8 _pad;
	__u16 port; // host order; 0 = any
};

struct {
	__uint(type, BPF_MAP_TYPE_LPM_TRIE);
	__uint(max_entries, 4096);
	__type(key, struct policy_key);
	__type(value, struct policy_val);
	__uint(map_flags, BPF_F_NO_PREALLOC);
} egress_policy SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_LPM_TRIE);
	__uint(max_entries, 4096);
	__type(key, struct policy_key);
	__type(value, struct policy_val);
	__uint(map_flags, BPF_F_NO_PREALLOC);
} ingress_policy SEC(".maps");

// nat_event mirrors the Go decoder in nat_linux.go. Addresses are network order
// (ready for net.IP); ports are network order too (decoder swaps).
struct nat_event {
	__u32 orig_saddr;
	__u32 orig_daddr;
	__u32 new_saddr;
	__u32 new_daddr;
	__u32 vm_ip;
	__u16 orig_sport;
	__u16 orig_dport;
	__u16 new_sport;
	__u16 new_dport;
	__u16 len;
	__u8 proto;
	__u8 dir;     // EV_*
	__u8 verdict; // 0 = forwarded, 1 = dropped
	__u8 _pad[3];
};

// Force struct nat_event into BTF: it is only used as a local (the ringbuf
// carries it untyped), so without a reachable reference clang would prune it and
// bpf2go's `-type nat_event` lookup would fail.
struct nat_event *_unused_nat_event __attribute__((unused));

struct {
	__uint(type, BPF_MAP_TYPE_RINGBUF);
	__uint(max_entries, 1 << 20); // 1 MiB
} events SEC(".maps");

// Per-VM counters, keyed by vm_ip (network order).
struct nat_stats {
	__u64 rx_pkts;  // toward the VM
	__u64 rx_bytes;
	__u64 tx_pkts;  // from the VM
	__u64 tx_bytes;
	__u64 drops;
	__u64 conns;    // new flows created on the VM's behalf
};

struct {
	__uint(type, BPF_MAP_TYPE_PERCPU_HASH);
	__uint(max_entries, 4096);
	__type(key, __u32); // vm_ip, network order
	__type(value, struct nat_stats);
} stats SEC(".maps");

// ---- Small helpers ---------------------------------------------------------

static __always_inline struct global_config *get_cfg(void)
{
	__u32 zero = 0;
	return bpf_map_lookup_elem(&global_config_map, &zero);
}

static __always_inline struct nat_stats *stat_for(__u32 vm_ip)
{
	struct nat_stats *s = bpf_map_lookup_elem(&stats, &vm_ip);
	if (s)
		return s;
	struct nat_stats zero = {};
	bpf_map_update_elem(&stats, &vm_ip, &zero, BPF_NOEXIST);
	return bpf_map_lookup_elem(&stats, &vm_ip);
}

// L4 header offsets within the packet (from skb data base).
struct l4off {
	__u32 ip_off;   // start of IP header
	__u32 l4_off;   // start of TCP/UDP header
	__u8 proto;
	__u16 sport;    // network order
	__u16 dport;    // network order
	__u32 saddr;    // network order
	__u32 daddr;    // network order
	__u16 tot_len;  // host order
};

// parse validates eth+ipv4+l4 and fills out. Returns 1 on a TCP/UDP IPv4
// packet we handle, 0 to pass untouched, -1 to drop (non-first fragment).
static __always_inline int parse(struct __sk_buff *skb, struct l4off *o)
{
	void *data = (void *)(long)skb->data;
	void *data_end = (void *)(long)skb->data_end;

	struct ethhdr *eth = data;
	if ((void *)(eth + 1) > data_end)
		return 0;
	if (eth->h_proto != bpf_htons(ETH_P_IP))
		return 0;

	struct iphdr *ip = (void *)(eth + 1);
	if ((void *)(ip + 1) > data_end)
		return 0;

	__u8 proto = ip->protocol;
	if (proto != IPPROTO_TCP && proto != IPPROTO_UDP)
		return 0;

	// Fragmentation: only the first fragment carries the L4 header. Pass the
	// first fragment (MF set, offset 0) through; drop later fragments — NAT of
	// them needs reassembly we don't do yet.
	__u16 frag = bpf_ntohs(ip->frag_off);
	if (frag & IP_OFFMASK)
		return -1;

	__u32 ihl = ip->ihl * 4;
	if (ihl < sizeof(*ip))
		return 0;
	void *l4 = (void *)ip + ihl;

	o->ip_off = ETH_HLEN;
	o->l4_off = ETH_HLEN + ihl;
	o->proto = proto;
	o->saddr = ip->saddr;
	o->daddr = ip->daddr;
	o->tot_len = bpf_ntohs(ip->tot_len);

	if (proto == IPPROTO_TCP) {
		struct tcphdr *t = l4;
		if ((void *)(t + 1) > data_end)
			return 0;
		o->sport = t->source;
		o->dport = t->dest;
	} else {
		struct udphdr *u = l4;
		if ((void *)(u + 1) > data_end)
			return 0;
		o->sport = u->source;
		o->dport = u->dest;
	}
	return 1;
}

// l4_csum_off returns the byte offset of the L4 checksum field, or 0 if the
// protocol carries none we must fix (it always does for TCP/UDP here).
static __always_inline __u32 l4_csum_off(struct l4off *o)
{
	if (o->proto == IPPROTO_TCP)
		return o->l4_off + offsetof(struct tcphdr, check);
	return o->l4_off + offsetof(struct udphdr, check);
}

// set_addr rewrites a 32-bit IP field at off and fixes both the IP header
// checksum and (with the pseudo-header flag) the L4 checksum.
static __always_inline int set_addr(struct __sk_buff *skb, struct l4off *o,
				    __u32 off, __u32 old_ip, __u32 new_ip)
{
	if (old_ip == new_ip)
		return 0;
	int rc = bpf_skb_store_bytes(skb, off, &new_ip, sizeof(new_ip), 0);
	if (rc < 0)
		return rc;
	rc = bpf_l3_csum_replace(skb, o->ip_off + offsetof(struct iphdr, check),
				 old_ip, new_ip, sizeof(new_ip));
	if (rc < 0)
		return rc;
	__u32 l4c = l4_csum_off(o);
	// UDP with a zero checksum means "no checksum" — leave it zero.
	if (o->proto == IPPROTO_UDP) {
		__u16 cur = 0;
		bpf_skb_load_bytes(skb, l4c, &cur, sizeof(cur));
		if (cur == 0)
			return 0;
	}
	return bpf_l4_csum_replace(skb, l4c, old_ip, new_ip,
				   BPF_F_PSEUDO_HDR | sizeof(new_ip));
}

// set_port rewrites a 16-bit L4 port at off and fixes the L4 checksum.
static __always_inline int set_port(struct __sk_buff *skb, struct l4off *o,
				    __u32 off, __u16 old_port, __u16 new_port)
{
	if (old_port == new_port)
		return 0;
	int rc = bpf_skb_store_bytes(skb, off, &new_port, sizeof(new_port), 0);
	if (rc < 0)
		return rc;
	__u32 l4c = l4_csum_off(o);
	if (o->proto == IPPROTO_UDP) {
		__u16 cur = 0;
		bpf_skb_load_bytes(skb, l4c, &cur, sizeof(cur));
		if (cur == 0)
			return 0;
	}
	return bpf_l4_csum_replace(skb, l4c, old_port, new_port, sizeof(new_port));
}

static __always_inline __u32 sport_off(struct l4off *o)
{
	// source port is the first L4 field for both TCP and UDP.
	return o->l4_off + 0;
}
static __always_inline __u32 dport_off(struct l4off *o)
{
	return o->l4_off + 2;
}

// set_eth_dst rewrites the destination MAC (for frames we redirect into a TAP).
static __always_inline int set_eth_dst(struct __sk_buff *skb, const __u8 mac[ETH_ALEN])
{
	return bpf_skb_store_bytes(skb, offsetof(struct ethhdr, h_dest), mac, ETH_ALEN, 0);
}

// fill a bpf_sock_tuple (ipv4) from an (saddr,daddr,sport,dport) — all network
// order — for a conntrack lookup/alloc.
static __always_inline void mk_tuple(struct bpf_sock_tuple *t, __u32 saddr,
				     __u32 daddr, __u16 sport, __u16 dport)
{
	__builtin_memset(t, 0, sizeof(*t));
	t->ipv4.saddr = saddr;
	t->ipv4.daddr = daddr;
	t->ipv4.sport = sport;
	t->ipv4.dport = dport;
}

static __always_inline void emit(struct l4off *o, __u32 vm_ip, __u8 dir,
				 __u8 verdict, __u32 new_saddr, __u32 new_daddr,
				 __u16 new_sport, __u16 new_dport)
{
	struct nat_event *e = bpf_ringbuf_reserve(&events, sizeof(*e), 0);
	if (!e)
		return;
	e->orig_saddr = o->saddr;
	e->orig_daddr = o->daddr;
	e->new_saddr = new_saddr;
	e->new_daddr = new_daddr;
	e->vm_ip = vm_ip;
	e->orig_sport = o->sport;
	e->orig_dport = o->dport;
	e->new_sport = new_sport;
	e->new_dport = new_dport;
	e->len = o->tot_len;
	e->proto = o->proto;
	e->dir = dir;
	e->verdict = verdict;
	e->_pad[0] = e->_pad[1] = e->_pad[2] = 0;
	bpf_ringbuf_submit(e, 0);
}

// policy_check returns 1 (allow) / 0 (deny) for an address+port against a trie,
// falling back to def_allow on a miss.
static __always_inline int policy_check(void *trie, __u32 addr, __u16 dport_host,
					__u8 def_allow)
{
	struct policy_key k = {.prefixlen = 32, .addr = addr};
	struct policy_val *v = bpf_map_lookup_elem(trie, &k);
	if (!v)
		return def_allow;
	if (v->port != 0 && v->port != dport_host)
		return def_allow;
	return v->verdict ? 1 : 0;
}

// ---- nat_tap: TCX ingress on every TAP (VM-originated packets) -------------
//
// Handles new egress (SNAT to HOST_IP) and replies to inbound flows (un-DNAT),
// disambiguated by the conntrack lookup direction. ARP requests for the gateway
// are answered in place.

static __always_inline int handle_arp(struct __sk_buff *skb, struct global_config *cfg);

SEC("tc")
int nat_tap(struct __sk_buff *skb)
{
	struct global_config *cfg = get_cfg();
	if (!cfg)
		return TC_ACT_OK;

	// ARP for the gateway (guest resolving its default route).
	void *data = (void *)(long)skb->data;
	void *data_end = (void *)(long)skb->data_end;
	struct ethhdr *eth = data;
	if ((void *)(eth + 1) <= data_end && eth->h_proto == bpf_htons(ETH_P_ARP))
		return handle_arp(skb, cfg);

	struct l4off o = {};
	int p = parse(skb, &o);
	if (p == 0)
		return TC_ACT_OK;
	if (p < 0) { // non-first fragment
		emit(&o, o.saddr, EV_DENY, 1, 0, 0, 0, 0);
		return TC_ACT_SHOT;
	}

	__u32 vm_ip = o.saddr; // on the TAP, the VM is always the source
	struct nat_stats *st = stat_for(vm_ip);

	// Egress ACL on the true public destination (before any rewrite).
	if (!policy_check(&egress_policy, o.daddr, bpf_ntohs(o.dport),
			  cfg->default_egress_allow)) {
		if (st) st->drops++;
		emit(&o, vm_ip, EV_DENY, 1, 0, 0, 0, 0);
		return TC_ACT_SHOT;
	}

	struct bpf_sock_tuple tup;
	mk_tuple(&tup, o.saddr, o.daddr, o.sport, o.dport);
	struct bpf_ct_opts opts = {
		.netns_id = BPF_F_CURRENT_NETNS,
		.l4proto = o.proto,
	};

	struct nf_conn *ct = bpf_skb_ct_lookup(skb, &tup, sizeof(tup.ipv4), &opts, sizeof(opts));
	if (!ct) {
		// New egress flow: allocate, reserve a SNAT source binding (port 0
		// => kernel picks a free source port), insert.
		struct nf_conn___init *nfct = bpf_skb_ct_alloc(skb, &tup, sizeof(tup.ipv4),
							       &opts, sizeof(opts));
		if (!nfct) {
			if (st) st->drops++;
			return TC_ACT_SHOT;
		}
		union nf_inet_addr src = {};
		src.ip = cfg->host_ip;
		bpf_ct_set_nat_info(nfct, &src, 0, NF_NAT_MANIP_SRC);
		bpf_ct_set_timeout(nfct, CT_TIMEOUT * 1000); // ms
		bpf_ct_set_status(nfct, IPS_CONFIRMED);
		ct = bpf_ct_insert_entry(nfct);
		if (!ct) {
			// Lost an insert race: re-lookup and use the winner.
			ct = bpf_skb_ct_lookup(skb, &tup, sizeof(tup.ipv4), &opts, sizeof(opts));
			if (!ct) {
				if (st) st->drops++;
				return TC_ACT_SHOT;
			}
		} else if (st) {
			st->conns++;
		}
	}

	// opts.dir tells us whether this packet is the original or reply side.
	__u8 dir = opts.dir;

	// For an original-direction packet on an egress SNAT flow, the new source is
	// the reply tuple's destination (HOST_IP : allocated source port). For a
	// reply-direction packet (a reply to an *inbound* DNAT flow), the source must
	// be restored to the original tuple's destination (HOST_IP : host_port) — the
	// public endpoint the remote client originally connected to.
	__u32 snat_addr = BPF_CORE_READ(ct, tuplehash[CT_DIR_REPLY].tuple.dst.u3.ip);
	__u16 snat_port = BPF_CORE_READ(ct, tuplehash[CT_DIR_REPLY].tuple.dst.u.all);
	__u32 undnat_addr = BPF_CORE_READ(ct, tuplehash[CT_DIR_ORIGINAL].tuple.dst.u3.ip);
	__u16 undnat_port = BPF_CORE_READ(ct, tuplehash[CT_DIR_ORIGINAL].tuple.dst.u.all);
	bpf_ct_release(ct);

	int rc;
	if (dir == CT_DIR_ORIGINAL) {
		// Egress SNAT: src = HOST_IP : allocated port.
		rc = set_addr(skb, &o, o.ip_off + offsetof(struct iphdr, saddr),
			      o.saddr, snat_addr);
		if (rc < 0) goto drop;
		rc = set_port(skb, &o, sport_off(&o), o.sport, snat_port);
		if (rc < 0) goto drop;
		emit(&o, vm_ip, EV_EGRESS, 0, snat_addr, o.daddr, snat_port, o.dport);
	} else {
		// Reply to an inbound flow: un-DNAT the source back to the original
		// public (HOST_IP : host_port).
		rc = set_addr(skb, &o, o.ip_off + offsetof(struct iphdr, saddr),
			      o.saddr, undnat_addr);
		if (rc < 0) goto drop;
		rc = set_port(skb, &o, sport_off(&o), o.sport, undnat_port);
		if (rc < 0) goto drop;
		emit(&o, vm_ip, EV_INBOUND_REPLY, 0, undnat_addr, o.daddr, undnat_port, o.dport);
	}

	if (st) {
		st->tx_pkts++;
		st->tx_bytes += o.tot_len;
	}
	// Outbound: let the kernel do FIB + neighbor resolution to the real gateway
	// and fill in L2 on the uplink.
	return bpf_redirect_neigh(cfg->uplink_ifindex, NULL, 0, 0);

drop:
	if (st) st->drops++;
	return TC_ACT_SHOT;
}

// ---- nat_uplink: TCX ingress on the host uplink (internet-originated) -------
//
// Handles replies to egress flows (un-SNAT) and new inbound connections to a
// published host port (DNAT). A conntrack/dnat miss passes the packet to the
// host stack untouched so the host's own services and SSH keep working.

SEC("tc")
int nat_uplink(struct __sk_buff *skb)
{
	struct global_config *cfg = get_cfg();
	if (!cfg)
		return TC_ACT_OK;

	struct l4off o = {};
	int p = parse(skb, &o);
	if (p == 0)
		return TC_ACT_OK;
	if (p < 0)
		return TC_ACT_SHOT; // non-first fragment of NAT'd traffic

	struct bpf_sock_tuple tup;
	mk_tuple(&tup, o.saddr, o.daddr, o.sport, o.dport);
	struct bpf_ct_opts opts = {
		.netns_id = BPF_F_CURRENT_NETNS,
		.l4proto = o.proto,
	};

	struct nf_conn *ct = bpf_skb_ct_lookup(skb, &tup, sizeof(tup.ipv4), &opts, sizeof(opts));
	if (ct && opts.dir == CT_DIR_REPLY) {
		// Reply to an egress (SNAT) flow: un-SNAT dst back to VM_IP:vm_port.
		__u32 vm_ip = BPF_CORE_READ(ct, tuplehash[CT_DIR_ORIGINAL].tuple.src.u3.ip);
		__u16 vm_port = BPF_CORE_READ(ct, tuplehash[CT_DIR_ORIGINAL].tuple.src.u.all);
		bpf_ct_release(ct);

		struct vm_entry *vm = bpf_map_lookup_elem(&vm_config, &vm_ip);
		if (!vm)
			return TC_ACT_OK; // unknown VM; not ours

		int rc = set_addr(skb, &o, o.ip_off + offsetof(struct iphdr, daddr),
				  o.daddr, vm_ip);
		if (rc < 0) return TC_ACT_SHOT;
		rc = set_port(skb, &o, dport_off(&o), o.dport, vm_port);
		if (rc < 0) return TC_ACT_SHOT;
		if (set_eth_dst(skb, vm->vm_mac) < 0)
			return TC_ACT_SHOT;

		struct nat_stats *st = stat_for(vm_ip);
		if (st) { st->rx_pkts++; st->rx_bytes += o.tot_len; }
		emit(&o, vm_ip, EV_EGRESS_REPLY, 0, o.saddr, vm_ip, o.sport, vm_port);
		return bpf_redirect(vm->tap_ifindex, 0);
	}
	if (ct)
		bpf_ct_release(ct);

	// ---- new inbound: is there a published-port rule? ----
	struct dnat_key dk = {
		.host_ip = o.daddr,
		.host_port = o.dport,
		.proto = o.proto,
	};
	struct dnat_val *dn = bpf_map_lookup_elem(&dnat_rules, &dk);
	if (!dn)
		return TC_ACT_OK; // not published; hand to the host stack

	// Ingress ACL on the true remote source (before DNAT).
	if (!policy_check(&ingress_policy, o.saddr, bpf_ntohs(o.dport),
			  cfg->default_ingress_allow)) {
		struct nat_stats *st = stat_for(dn->vm_ip);
		if (st) st->drops++;
		emit(&o, dn->vm_ip, EV_DENY, 1, 0, 0, 0, 0);
		return TC_ACT_SHOT;
	}

	struct nf_conn___init *nfct = bpf_skb_ct_alloc(skb, &tup, sizeof(tup.ipv4),
						       &opts, sizeof(opts));
	if (!nfct)
		return TC_ACT_SHOT;
	union nf_inet_addr dst = {};
	dst.ip = dn->vm_ip;
	bpf_ct_set_nat_info(nfct, &dst, bpf_ntohs(dn->vm_port), NF_NAT_MANIP_DST);
	bpf_ct_set_timeout(nfct, CT_TIMEOUT * 1000);
	bpf_ct_set_status(nfct, IPS_CONFIRMED);
	struct nf_conn *ins = bpf_ct_insert_entry(nfct);
	if (ins)
		bpf_ct_release(ins);

	// DNAT rewrite: dst = VM_IP : vm_port; L2 dst = VM_MAC.
	int rc = set_addr(skb, &o, o.ip_off + offsetof(struct iphdr, daddr),
			  o.daddr, dn->vm_ip);
	if (rc < 0) return TC_ACT_SHOT;
	rc = set_port(skb, &o, dport_off(&o), o.dport, dn->vm_port);
	if (rc < 0) return TC_ACT_SHOT;
	if (set_eth_dst(skb, dn->vm_mac) < 0)
		return TC_ACT_SHOT;

	struct nat_stats *st = stat_for(dn->vm_ip);
	if (st) { st->rx_pkts++; st->rx_bytes += o.tot_len; st->conns++; }
	emit(&o, dn->vm_ip, EV_INBOUND, 0, o.saddr, dn->vm_ip, o.sport, dn->vm_port);
	return bpf_redirect(dn->tap_ifindex, 0);
}

// ---- ARP responder ---------------------------------------------------------
//
// The guest ARPs for the gateway, which is owned by no host interface. We craft
// an in-place reply (GW_IP is-at gw_mac) by swapping the request around and
// bouncing it back out the TAP it arrived on. arphdr here is the fixed header;
// the IPv4/Ethernet addresses follow it (we access them by offset).

struct arp_eth_ipv4 {
	__u8 ar_sha[ETH_ALEN]; // sender hw addr
	__u8 ar_sip[4];        // sender ip
	__u8 ar_tha[ETH_ALEN]; // target hw addr
	__u8 ar_tip[4];        // target ip
};

static __always_inline int handle_arp(struct __sk_buff *skb, struct global_config *cfg)
{
	void *data = (void *)(long)skb->data;
	void *data_end = (void *)(long)skb->data_end;

	struct ethhdr *eth = data;
	struct arphdr *arp = (void *)(eth + 1);
	if ((void *)(arp + 1) > data_end)
		return TC_ACT_OK;
	if (arp->ar_op != bpf_htons(ARPOP_REQUEST) ||
	    arp->ar_hrd != bpf_htons(ARPHRD_ETHER) ||
	    arp->ar_pro != bpf_htons(ETH_P_IP))
		return TC_ACT_OK;

	struct arp_eth_ipv4 *a = (void *)(arp + 1);
	if ((void *)(a + 1) > data_end)
		return TC_ACT_OK;

	__u32 target;
	__builtin_memcpy(&target, a->ar_tip, 4);
	if (target != cfg->gw_ip)
		return TC_ACT_OK; // only answer for the gateway

	// Build the reply in place: opcode REPLY; sender := gateway; target :=
	// original sender. Then swap Ethernet src/dst and bounce back out the TAP.
	__u16 reply_op = bpf_htons(ARPOP_REPLY);
	bpf_skb_store_bytes(skb, ETH_HLEN + offsetof(struct arphdr, ar_op),
			    &reply_op, sizeof(reply_op), 0);

	__u32 arp_payload = ETH_HLEN + sizeof(struct arphdr);
	__u8 req_sha[ETH_ALEN];
	__u8 req_sip[4];
	bpf_skb_load_bytes(skb, arp_payload + offsetof(struct arp_eth_ipv4, ar_sha),
			   req_sha, ETH_ALEN);
	bpf_skb_load_bytes(skb, arp_payload + offsetof(struct arp_eth_ipv4, ar_sip),
			   req_sip, 4);

	// target hw/ip := original sender's
	bpf_skb_store_bytes(skb, arp_payload + offsetof(struct arp_eth_ipv4, ar_tha),
			    req_sha, ETH_ALEN, 0);
	bpf_skb_store_bytes(skb, arp_payload + offsetof(struct arp_eth_ipv4, ar_tip),
			    req_sip, 4, 0);
	// sender hw/ip := gateway
	bpf_skb_store_bytes(skb, arp_payload + offsetof(struct arp_eth_ipv4, ar_sha),
			    cfg->gw_mac, ETH_ALEN, 0);
	bpf_skb_store_bytes(skb, arp_payload + offsetof(struct arp_eth_ipv4, ar_sip),
			    &cfg->gw_ip, 4, 0);

	// Ethernet: dst := requester, src := gateway.
	bpf_skb_store_bytes(skb, offsetof(struct ethhdr, h_dest), req_sha, ETH_ALEN, 0);
	bpf_skb_store_bytes(skb, offsetof(struct ethhdr, h_source), cfg->gw_mac, ETH_ALEN, 0);

	// Bounce back toward the guest: redirecting to the TAP's egress path (flag 0)
	// transmits the frame out the TAP, which the guest receives. (This responder
	// is belt-and-suspenders — the guest also gets a static gateway neighbor from
	// the init agent — but must still send the reply the correct way.)
	return bpf_redirect(skb->ifindex, 0);
}
