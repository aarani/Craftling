//go:build bpf && linux

// End-to-end tests for the eBPF networking dataplane (the `nat_tap` /
// `nat_uplink` NAT programs and the `tapfilter` observe/drop program). Unlike
// the unit tests, these load the *real* compiled eBPF objects into the running
// kernel, attach them to real network devices, and drive real packets through
// them — so they catch verifier regressions, kfunc-resolution failures, and
// userspace<->kernel struct-layout drift that a pure-Go test cannot.
//
// They need root (CAP_BPF + CAP_NET_ADMIN), a kernel >= 6.6 (TCX links and the
// nf_conntrack bpf_ct_* kfuncs the NAT programs rely on), and the `ip` tool for
// the veth/netns plumbing. They are gated behind the `bpf` build tag so the
// default `go test ./...` lane (which has neither root nor a guaranteed kernel)
// never compiles or runs them. Run them with:
//
//	sudo -E env "PATH=$PATH" go test -tags bpf -count=1 -v ./internal/agent/firecracker/...
//
// This file holds the shared scaffolding: capability/kernel gating, veth+netns
// setup driven through `ip`, and the packet builders the TEST_RUN cases feed to
// the programs.
package firecracker

import (
	"encoding/binary"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// tc action codes (linux/pkt_cls.h) the programs return.
const (
	tcActOK       = 0
	tcActShot     = 2
	tcActRedirect = 7
)

// EtherType / ARP constants used by the packet builders.
const (
	ethPIP  = 0x0800
	ethPARP = 0x0806

	arpHWEther   = 1
	arpOpRequest = 1
	arpOpReply   = 2
)

// requireBPFRoot skips the test unless it can actually exercise the dataplane:
// it needs to be root (eBPF load + TCX attach + netns), and a kernel new enough
// for TCX and the nf_conntrack kfuncs. On a supported kernel a later load
// failure is a real failure (a regression we want to catch), not a skip.
func requireBPFRoot(t *testing.T) {
	t.Helper()
	if syscall.Geteuid() != 0 {
		t.Skip("eBPF dataplane e2e needs root (CAP_BPF + CAP_NET_ADMIN); run under sudo")
	}
	requireKernel(t, 6, 6)
	// Best-effort: bring up nf_conntrack/nf_nat so the NAT programs' kfuncs
	// resolve at load time. Mirrors what the production dataplane does.
	_ = ensureConntrack()
}

// requireKernel skips when the running kernel is older than major.minor. The
// NAT programs need TCX (6.6) and the conntrack kfuncs that landed alongside it;
// below that the load is expected to fail, so skipping keeps the suite green on
// old runners while still failing on supported ones.
func requireKernel(t *testing.T, major, minor int) {
	t.Helper()
	var u unix.Utsname
	if err := unix.Uname(&u); err != nil {
		t.Skipf("uname: %v", err)
	}
	rel := string(u.Release[:])
	if i := strings.IndexByte(rel, 0); i >= 0 {
		rel = rel[:i]
	}
	maj, min := parseKernel(rel)
	if maj < major || (maj == major && min < minor) {
		t.Skipf("kernel %s < %d.%d; eBPF dataplane needs TCX + conntrack kfuncs", rel, major, minor)
	}
}

// parseKernel extracts the leading major.minor from a uname release string like
// "7.0.0-14-generic". Returns (0, 0) if it can't be parsed (caller treats that
// as "too old" and skips).
func parseKernel(rel string) (major, minor int) {
	parts := strings.SplitN(rel, ".", 3)
	if len(parts) < 2 {
		return 0, 0
	}
	major, _ = strconv.Atoi(parts[0])
	// The minor component can carry a suffix on some distros; trim non-digits.
	m := parts[1]
	for i := 0; i < len(m); i++ {
		if m[i] < '0' || m[i] > '9' {
			m = m[:i]
			break
		}
	}
	minor, _ = strconv.Atoi(m)
	return major, minor
}

// ---- veth + netns plumbing (driven through `ip`) ---------------------------

// vethNS is a veth pair with the peer end isolated in its own network
// namespace. The host end (Host) stays in the root namespace so a TCX program
// attached to it sees packets the host transmits toward, or receives from, the
// peer. Cleanup tears the whole thing down.
type vethNS struct {
	Host   string // host-side veth interface name (root netns)
	Peer   string // peer-side veth interface name (inside NS)
	NS     string // network namespace name
	HostIP net.IP // address on Host
	PeerIP net.IP // address on Peer (inside NS)
}

// setupVethNS creates a veth pair, moves the peer into a fresh netns, addresses
// both ends out of subnet (a /24, e.g. "10.250.1"), and brings everything up.
// Names are derived from tag (kept short for IFNAMSIZ). It registers a cleanup
// that removes the namespace and the host veth.
func setupVethNS(t *testing.T, tag, subnet24 string) vethNS {
	t.Helper()
	v := vethNS{
		Host:   "cfh" + tag,
		Peer:   "cfp" + tag,
		NS:     "cfns" + tag,
		HostIP: net.ParseIP(subnet24 + ".1"),
		PeerIP: net.ParseIP(subnet24 + ".2"),
	}
	if len(v.Host) >= unix.IFNAMSIZ || len(v.Peer) >= unix.IFNAMSIZ {
		t.Fatalf("interface name too long for tag %q", tag)
	}

	// Pre-clean any leftovers from a previous crashed run, then build.
	_ = exec.Command("ip", "netns", "del", v.NS).Run()
	_ = exec.Command("ip", "link", "del", v.Host).Run()

	steps := [][]string{
		{"netns", "add", v.NS},
		{"link", "add", v.Host, "type", "veth", "peer", "name", v.Peer},
		{"link", "set", v.Peer, "netns", v.NS},
		{"addr", "add", v.HostIP.String() + "/24", "dev", v.Host},
		{"link", "set", v.Host, "up"},
		{"netns", "exec", v.NS, "ip", "addr", "add", v.PeerIP.String() + "/24", "dev", v.Peer},
		{"netns", "exec", v.NS, "ip", "link", "set", v.Peer, "up"},
		{"netns", "exec", v.NS, "ip", "link", "set", "lo", "up"},
	}
	for _, s := range steps {
		if out, err := exec.Command("ip", s...).CombinedOutput(); err != nil {
			// Roll back what we created so a missing `ip` feature skips cleanly.
			_ = exec.Command("ip", "netns", "del", v.NS).Run()
			_ = exec.Command("ip", "link", "del", v.Host).Run()
			t.Skipf("ip %s: %v (%s)", strings.Join(s, " "), err, strings.TrimSpace(string(out)))
		}
	}
	t.Cleanup(func() {
		_ = exec.Command("ip", "netns", "del", v.NS).Run()
		_ = exec.Command("ip", "link", "del", v.Host).Run()
	})
	return v
}

// ifIndex returns the kernel ifindex of a (root-netns) interface by name.
func ifIndex(t *testing.T, name string) int {
	t.Helper()
	iface, err := net.InterfaceByName(name)
	if err != nil {
		t.Fatalf("InterfaceByName(%q): %v", name, err)
	}
	return iface.Index
}

// ---- packet builders (for BPF_PROG_TEST_RUN) -------------------------------

var (
	macA = net.HardwareAddr{0x02, 0x00, 0x00, 0x00, 0x00, 0xaa}
	macB = net.HardwareAddr{0x02, 0x00, 0x00, 0x00, 0x00, 0xbb}
)

// ethIPv4 builds an Ethernet+IPv4 frame carrying l4 with the given protocol.
// Checksums are left zero: the tapfilter program never validates them and
// BPF_PROG_TEST_RUN does not either, so they don't matter for these tests.
func ethIPv4(src, dst net.HardwareAddr, sip, dip net.IP, proto byte, l4 []byte) []byte {
	eth := make([]byte, 14)
	copy(eth[0:6], dst)
	copy(eth[6:12], src)
	binary.BigEndian.PutUint16(eth[12:14], ethPIP)

	ip := make([]byte, 20)
	ip[0] = 0x45 // version 4, IHL 5
	binary.BigEndian.PutUint16(ip[2:4], uint16(20+len(l4)))
	ip[8] = 64 // TTL
	ip[9] = proto
	copy(ip[12:16], sip.To4())
	copy(ip[16:20], dip.To4())

	return concat(eth, ip, l4)
}

// udpSeg builds a minimal UDP header + payload. Ports are host order in the
// arguments; written network order on the wire.
func udpSeg(sport, dport uint16, payload []byte) []byte {
	u := make([]byte, 8)
	binary.BigEndian.PutUint16(u[0:2], sport)
	binary.BigEndian.PutUint16(u[2:4], dport)
	binary.BigEndian.PutUint16(u[4:6], uint16(8+len(payload)))
	return append(u, payload...)
}

// tcpSeg builds a minimal (header-only) TCP segment with data offset 5.
func tcpSeg(sport, dport uint16) []byte {
	t := make([]byte, 20)
	binary.BigEndian.PutUint16(t[0:2], sport)
	binary.BigEndian.PutUint16(t[2:4], dport)
	t[12] = 5 << 4 // data offset = 5 words, no options
	t[13] = 0x02   // SYN
	return t
}

// arpRequestFrame builds a broadcast ARP "who-has tip, tell sip" request from
// srcMAC. nat_tap's responder answers it for the configured gateway.
func arpRequestFrame(srcMAC net.HardwareAddr, sip, tip net.IP) []byte {
	eth := make([]byte, 14)
	for i := range eth[0:6] {
		eth[i] = 0xff // broadcast
	}
	copy(eth[6:12], srcMAC)
	binary.BigEndian.PutUint16(eth[12:14], ethPARP)

	arp := make([]byte, 8)
	binary.BigEndian.PutUint16(arp[0:2], arpHWEther)
	binary.BigEndian.PutUint16(arp[2:4], ethPIP)
	arp[4] = 6 // hardware addr len
	arp[5] = 4 // protocol addr len
	binary.BigEndian.PutUint16(arp[6:8], arpOpRequest)

	payload := make([]byte, 20)
	copy(payload[0:6], srcMAC)      // sender hw
	copy(payload[6:10], sip.To4())  // sender ip
	copy(payload[16:20], tip.To4()) // target ip (target hw left zero)

	return concat(eth, arp, payload)
}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// nativeU32 loads an IPv4 address as a host-endian uint32 whose in-memory bytes
// are the address in network order — matching how the eBPF programs and the
// bpf2go-generated structs (which type network-order __u32 fields as uint32)
// see it.
func nativeU32(ip net.IP) uint32 {
	return binary.NativeEndian.Uint32(ip.To4())
}

func mac6(m net.HardwareAddr) [6]byte {
	var a [6]byte
	copy(a[:], m)
	return a
}

// be16Native returns the host-endian uint16 whose in-memory bytes are v in
// network order — what a bpf2go-generated uint16 field that actually stores a
// network-order port reads back as.
func be16Native(v uint16) uint16 {
	var b [2]byte
	binary.BigEndian.PutUint16(b[:], v)
	return binary.NativeEndian.Uint16(b[:])
}

// mustParseMAC is a fatal-on-error net.ParseMAC for test fixtures.
func mustParseMAC(t *testing.T, s string) net.HardwareAddr {
	t.Helper()
	m, err := net.ParseMAC(s)
	if err != nil {
		t.Fatalf("ParseMAC(%q): %v", s, err)
	}
	return m
}

// describeFrame is a small diagnostics helper for failing packet assertions.
func describeFrame(b []byte) string {
	if len(b) > 64 {
		b = b[:64]
	}
	return fmt.Sprintf("% x", b)
}
