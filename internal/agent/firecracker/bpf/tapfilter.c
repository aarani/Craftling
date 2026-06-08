//go:build ignore

// tapfilter is a tc/clsact (SCHED_CLS) program attached to both directions of a
// microVM's host TAP device via TCX. It watches a single TCP/UDP port (set from
// userspace through config_map) and, for every matching packet, bumps a stats
// counter and emits a flow event on the events ringbuf. If config.drop is set,
// matching packets are dropped (TC_ACT_SHOT); otherwise they pass (TC_ACT_OK).
// Non-matching packets always pass untouched.
//
// Direction note: on a TAP device the host's view is inverted relative to the
// guest. Packets the guest *sends* arrive as TAP ingress; packets bound *for*
// the guest leave as TAP egress. The `ingress` flag below is the host-side
// direction, so ingress==1 means "guest transmitted this".

#include <linux/bpf.h>
#include <linux/if_ether.h>
#include <linux/ip.h>
#include <linux/tcp.h>
#include <linux/udp.h>
#include <linux/pkt_cls.h>
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_endian.h>

char __license[] SEC("license") = "GPL";

// config is written from userspace (one entry, key 0). port is host byte order.
struct config {
	__u16 port; // watched TCP/UDP port; 0 disables matching entirely
	__u8 drop;  // 1 => drop matching packets
	__u8 _pad;
};

struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__uint(max_entries, 1);
	__type(key, __u32);
	__type(value, struct config);
} config_map SEC(".maps");

// event mirrors bpfEvent on the Go side (see -type event in gen.go).
struct event {
	__u32 saddr; // network byte order, as on the wire
	__u32 daddr; // network byte order
	__u16 sport; // host byte order
	__u16 dport; // host byte order
	__u16 len;   // IP total length
	__u8 ingress; // host-side direction: 1 = guest TX, 0 = host->guest
	__u8 dropped; // 1 if this packet was dropped
	__u8 proto;   // IPPROTO_TCP / IPPROTO_UDP
	__u8 _pad[3];
};

struct {
	__uint(type, BPF_MAP_TYPE_RINGBUF);
	__uint(max_entries, 1 << 20); // 1 MiB
} events SEC(".maps");

// stats indices: 0 = ingress matches, 1 = egress matches, 2 = dropped.
struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__uint(max_entries, 4);
	__type(key, __u32);
	__type(value, __u64);
} stats SEC(".maps");

static __always_inline void bump(__u32 idx)
{
	__u64 *c = bpf_map_lookup_elem(&stats, &idx);
	if (c)
		__sync_fetch_and_add(c, 1);
}

static __always_inline int handle(struct __sk_buff *skb, __u8 ingress)
{
	void *data = (void *)(long)skb->data;
	void *data_end = (void *)(long)skb->data_end;

	struct ethhdr *eth = data;
	if ((void *)(eth + 1) > data_end)
		return TC_ACT_OK;
	if (eth->h_proto != bpf_htons(ETH_P_IP))
		return TC_ACT_OK; // IPv4 only for now

	struct iphdr *ip = (void *)(eth + 1);
	if ((void *)(ip + 1) > data_end)
		return TC_ACT_OK;

	__u8 proto = ip->protocol;
	if (proto != IPPROTO_TCP && proto != IPPROTO_UDP)
		return TC_ACT_OK;

	__u32 ihl = ip->ihl * 4;
	if (ihl < sizeof(*ip))
		return TC_ACT_OK;
	void *l4 = (void *)ip + ihl;

	__u16 sport, dport;
	if (proto == IPPROTO_TCP) {
		struct tcphdr *t = l4;
		if ((void *)(t + 1) > data_end)
			return TC_ACT_OK;
		sport = bpf_ntohs(t->source);
		dport = bpf_ntohs(t->dest);
	} else {
		struct udphdr *u = l4;
		if ((void *)(u + 1) > data_end)
			return TC_ACT_OK;
		sport = bpf_ntohs(u->source);
		dport = bpf_ntohs(u->dest);
	}

	__u32 zero = 0;
	struct config *cfg = bpf_map_lookup_elem(&config_map, &zero);
	if (!cfg || cfg->port == 0)
		return TC_ACT_OK;
	if (sport != cfg->port && dport != cfg->port)
		return TC_ACT_OK;

	int drop = cfg->drop ? 1 : 0;

	bump(ingress ? 0 : 1);
	if (drop)
		bump(2);

	struct event *e = bpf_ringbuf_reserve(&events, sizeof(*e), 0);
	if (e) {
		e->saddr = ip->saddr;
		e->daddr = ip->daddr;
		e->sport = sport;
		e->dport = dport;
		e->len = bpf_ntohs(ip->tot_len);
		e->ingress = ingress;
		e->dropped = drop;
		e->proto = proto;
		e->_pad[0] = e->_pad[1] = e->_pad[2] = 0;
		bpf_ringbuf_submit(e, 0);
	}

	return drop ? TC_ACT_SHOT : TC_ACT_OK;
}

SEC("tc")
int tap_ingress(struct __sk_buff *skb)
{
	return handle(skb, 1);
}

SEC("tc")
int tap_egress(struct __sk_buff *skb)
{
	return handle(skb, 0);
}
