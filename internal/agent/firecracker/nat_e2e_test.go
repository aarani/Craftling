//go:build bpf && linux

package firecracker

import (
	"bytes"
	"errors"
	"net"
	"os/exec"
	"strings"
	"testing"

	"github.com/aarani/craftling-go/internal/agent/firecracker/bpf"
	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/rlimit"
	"golang.org/x/sys/unix"
)

// setupAddrVeth creates an addressed veth in the root namespace to stand in for
// the host uplink: the dataplane needs a real interface with a non-loopback
// IPv4 (primaryIPv4) to attach nat_uplink to and to use as the SNAT source. The
// peer end is left down and unused. Returns the host-side name.
func setupAddrVeth(t *testing.T, tag, addr24 string) (name string, hostIP net.IP) {
	t.Helper()
	host := "cfu" + tag
	peer := "cfx" + tag
	if len(host) >= unix.IFNAMSIZ || len(peer) >= unix.IFNAMSIZ {
		t.Fatalf("interface name too long for tag %q", tag)
	}
	hostIP = net.ParseIP(addr24 + ".1")

	_ = exec.Command("ip", "link", "del", host).Run()
	steps := [][]string{
		{"link", "add", host, "type", "veth", "peer", "name", peer},
		{"addr", "add", hostIP.String() + "/24", "dev", host},
		{"link", "set", host, "up"},
	}
	for _, s := range steps {
		if out, err := exec.Command("ip", s...).CombinedOutput(); err != nil {
			_ = exec.Command("ip", "link", "del", host).Run()
			t.Skipf("ip %s: %v (%s)", strings.Join(s, " "), err, strings.TrimSpace(string(out)))
		}
	}
	t.Cleanup(func() { _ = exec.Command("ip", "link", "del", host).Run() })
	return host, hostIP
}

// loadNat loads the compiled NAT objects into the kernel, asserting the verifier
// accepts both programs and the nf_conntrack bpf_ct_* kfuncs resolve. Returns
// the objects with a cleanup.
func loadNat(t *testing.T) *bpf.NatObjects {
	t.Helper()
	_ = rlimit.RemoveMemlock()
	objs := &bpf.NatObjects{}
	if err := bpf.LoadNatObjects(objs, nil); err != nil {
		t.Fatalf("load nat objects (kernel >= 6.6 + nf_conntrack/nf_nat kfuncs?): %v", err)
	}
	t.Cleanup(func() { _ = objs.Close() })
	return objs
}

// TestNATLoadAndVerify is the core on-kernel gate: it proves nat.c still
// verifies on the running kernel and that the conntrack kfuncs it calls resolve.
// This is the check the design doc flagged as "pending on-kernel verification".
func TestNATLoadAndVerify(t *testing.T) {
	requireBPFRoot(t)
	objs := loadNat(t)

	if objs.NatTap == nil || objs.NatUplink == nil {
		t.Fatal("nat_tap / nat_uplink program missing after load")
	}
	// All maps userspace populates must be present too.
	for name, m := range map[string]*ebpf.Map{
		"global_config_map": objs.GlobalConfigMap,
		"vm_config":         objs.VmConfig,
		"dnat_rules":        objs.DnatRules,
		"egress_policy":     objs.EgressPolicy,
		"ingress_policy":    objs.IngressPolicy,
		"events":            objs.Events,
		"stats":             objs.Stats,
	} {
		if m == nil {
			t.Fatalf("map %q missing after load", name)
		}
	}
}

// TestNATDataplaneLifecycle drives the production dataplane wiring against real
// devices: newDataplane attaches nat_uplink to an uplink veth, publishVM
// attaches nat_tap to a TAP and installs the per-VM map entries, and withdrawVM
// removes them. It asserts the map round-trips (proving the userspace struct
// layouts match the kernel's) and that teardown is clean.
func TestNATDataplaneLifecycle(t *testing.T) {
	requireBPFRoot(t)

	uplink, hostIP := setupAddrVeth(t, "lc", "192.0.2")
	cfg := Config{UplinkDevice: uplink}
	if err := cfg.validateDataplane(); err != nil { // fills subnet/gateway/port defaults
		t.Fatalf("validateDataplane: %v", err)
	}
	dc, err := cfg.dataplaneConfig()
	if err != nil {
		t.Fatalf("dataplaneConfig: %v", err)
	}

	dp, err := newDataplane(dc)
	if err != nil {
		t.Fatalf("newDataplane: %v", err)
	}
	defer dp.Close()

	// Global config must reflect the uplink we attached to.
	var gc bpf.NatGlobalConfig
	if err := dp.objs.GlobalConfigMap.Lookup(uint32(0), &gc); err != nil {
		t.Fatalf("read global config: %v", err)
	}
	if gc.UplinkIfindex != uint32(ifIndex(t, uplink)) {
		t.Fatalf("global uplink ifindex = %d, want %d", gc.UplinkIfindex, ifIndex(t, uplink))
	}
	if gc.HostIp != nativeU32(hostIP) {
		t.Fatalf("global host_ip = %#x, want %#x (%s)", gc.HostIp, nativeU32(hostIP), hostIP)
	}

	// Publish a VM on a real TAP.
	const tap = "cftaplc"
	if err := createTAP(tap); err != nil {
		t.Fatalf("createTAP: %v", err)
	}
	t.Cleanup(func() { _ = deleteTAP(tap) })

	const vmServicePort = 25565
	n := vmNet{
		VMIP:       net.IPv4(10, 222, 0, 5).To4(),
		VMMAC:      mustParseMAC(t, "02:00:0a:de:00:05"),
		HostPort:   30005,
		GatewayIP:  dc.gatewayIP,
		GatewayMAC: dc.gatewayMAC,
		PrefixLen:  16,
	}
	if err := dp.publishVM(tap, n, vmServicePort); err != nil {
		t.Fatalf("publishVM: %v", err)
	}
	tapIdx := uint32(ifIndex(t, tap))

	// vm_config round-trip: keyed by the VM IP (network order bytes).
	var vmKey [4]byte
	copy(vmKey[:], n.VMIP.To4())
	var vm bpf.NatVmEntry
	if err := dp.objs.VmConfig.Lookup(&vmKey, &vm); err != nil {
		t.Fatalf("read vm_config: %v", err)
	}
	if vm.TapIfindex != tapIdx {
		t.Fatalf("vm_config tap ifindex = %d, want %d", vm.TapIfindex, tapIdx)
	}
	if vm.VmMac != mac6(n.VMMAC) {
		t.Fatalf("vm_config mac = % x, want % x", vm.VmMac, mac6(n.VMMAC))
	}

	// dnat_rules round-trip: reconstruct the exact key publishVM wrote.
	dk := natDNATKey{HostPort: be16(n.HostPort), Proto: unix.IPPROTO_TCP}
	copy(dk.HostIP[:], dp.hostIP.To4())
	var dv bpf.NatDnatVal
	if err := dp.objs.DnatRules.Lookup(&dk, &dv); err != nil {
		t.Fatalf("read dnat_rules: %v", err)
	}
	if dv.TapIfindex != tapIdx {
		t.Fatalf("dnat tap ifindex = %d, want %d", dv.TapIfindex, tapIdx)
	}
	if dv.VmPort != be16Native(vmServicePort) {
		t.Fatalf("dnat vm_port = %#x, want %#x", dv.VmPort, be16Native(vmServicePort))
	}

	// publishVM is idempotent per TAP.
	if err := dp.publishVM(tap, n, vmServicePort); err != nil {
		t.Fatalf("second publishVM should be a no-op: %v", err)
	}

	// vmStats for an IP with no recorded flows reports not-found, not a zeroed hit.
	if _, ok := dp.vmStats(net.IPv4(10, 222, 99, 99)); ok {
		t.Fatal("vmStats for an unknown VM should report ok=false")
	}

	// withdrawVM removes both entries.
	dp.withdrawVM(tap, n)
	if err := dp.objs.VmConfig.Lookup(&vmKey, &vm); !errors.Is(err, ebpf.ErrKeyNotExist) {
		t.Fatalf("vm_config after withdraw: err = %v, want ErrKeyNotExist", err)
	}
	if err := dp.objs.DnatRules.Lookup(&dk, &dv); !errors.Is(err, ebpf.ErrKeyNotExist) {
		t.Fatalf("dnat_rules after withdraw: err = %v, want ErrKeyNotExist", err)
	}
}

// TestNATDataplaneConcurrentVMs publishes two VMs on two TAPs over the single
// shared collection and asserts their map entries are independent — the IPAM
// model is per-VM addressing over shared programs and maps.
func TestNATDataplaneConcurrentVMs(t *testing.T) {
	requireBPFRoot(t)

	uplink, _ := setupAddrVeth(t, "cc", "192.0.2")
	cfg := Config{UplinkDevice: uplink}
	if err := cfg.validateDataplane(); err != nil { // fills subnet/gateway/port defaults
		t.Fatalf("validateDataplane: %v", err)
	}
	dc, err := cfg.dataplaneConfig()
	if err != nil {
		t.Fatalf("dataplaneConfig: %v", err)
	}
	dp, err := newDataplane(dc)
	if err != nil {
		t.Fatalf("newDataplane: %v", err)
	}
	defer dp.Close()

	type vm struct {
		tap string
		n   vmNet
	}
	vms := []vm{
		{"cftapc1", vmNet{VMIP: net.IPv4(10, 222, 0, 11).To4(), VMMAC: mustParseMAC(t, "02:00:0a:de:00:0b"), HostPort: 30011, PrefixLen: 16}},
		{"cftapc2", vmNet{VMIP: net.IPv4(10, 222, 0, 12).To4(), VMMAC: mustParseMAC(t, "02:00:0a:de:00:0c"), HostPort: 30012, PrefixLen: 16}},
	}
	for i := range vms {
		if err := createTAP(vms[i].tap); err != nil {
			t.Fatalf("createTAP %s: %v", vms[i].tap, err)
		}
		tap := vms[i].tap
		t.Cleanup(func() { _ = deleteTAP(tap) })
		if err := dp.publishVM(vms[i].tap, vms[i].n, 25565); err != nil {
			t.Fatalf("publishVM %s: %v", vms[i].tap, err)
		}
	}

	for _, v := range vms {
		var key [4]byte
		copy(key[:], v.n.VMIP.To4())
		var e bpf.NatVmEntry
		if err := dp.objs.VmConfig.Lookup(&key, &e); err != nil {
			t.Fatalf("vm_config %s: %v", v.n.VMIP, err)
		}
		if want := uint32(ifIndex(t, v.tap)); e.TapIfindex != want {
			t.Fatalf("vm %s -> tap ifindex %d, want %d", v.n.VMIP, e.TapIfindex, want)
		}
		if e.VmMac != mac6(v.n.VMMAC) {
			t.Fatalf("vm %s mac = % x, want % x", v.n.VMIP, e.VmMac, mac6(v.n.VMMAC))
		}
	}
}

// TestNATTapARPResponder drives nat_tap's in-program ARP responder with
// BPF_PROG_TEST_RUN: a guest ARP "who-has <gateway>" must come back rewritten
// into a reply sourced from the gateway, redirected back out the TAP. A request
// for any other address must pass through untouched.
func TestNATTapARPResponder(t *testing.T) {
	requireBPFRoot(t)
	objs := loadNat(t)

	gwIP := net.IPv4(10, 222, 0, 1)
	gwMAC := mustParseMAC(t, "02:00:00:00:00:01")
	reqMAC := mustParseMAC(t, "02:00:0a:de:00:05")
	reqIP := net.IPv4(10, 222, 0, 5)

	// Configure the gateway the responder answers for.
	gc := bpf.NatGlobalConfig{GwIp: nativeU32(gwIP), GwMac: mac6(gwMAC)}
	if err := objs.GlobalConfigMap.Put(uint32(0), &gc); err != nil {
		t.Fatalf("write global config: %v", err)
	}

	t.Run("answers the gateway", func(t *testing.T) {
		req := arpRequestFrame(reqMAC, reqIP, gwIP)
		ret, out, err := objs.NatTap.Test(req)
		if err != nil {
			t.Fatalf("run nat_tap: %v", err)
		}
		if ret != tcActRedirect {
			t.Fatalf("verdict = %d, want TC_ACT_REDIRECT(%d)", ret, tcActRedirect)
		}
		if len(out) < 42 {
			t.Fatalf("output frame too short: %s", describeFrame(out))
		}
		// Ethernet: dst := requester, src := gateway.
		if !bytes.Equal(out[0:6], reqMAC) || !bytes.Equal(out[6:12], gwMAC) {
			t.Fatalf("eth dst/src = % x / % x, want % x / % x", out[0:6], out[6:12], reqMAC, gwMAC)
		}
		// ARP opcode REPLY.
		if op := uint16(out[20])<<8 | uint16(out[21]); op != arpOpReply {
			t.Fatalf("arp opcode = %d, want REPLY(%d)", op, arpOpReply)
		}
		// Sender hw/ip := gateway; target hw/ip := original requester.
		if !bytes.Equal(out[22:28], gwMAC) || !bytes.Equal(out[28:32], gwIP.To4()) {
			t.Fatalf("arp sender = % x / % x, want gateway % x / % x", out[22:28], out[28:32], gwMAC, gwIP.To4())
		}
		if !bytes.Equal(out[32:38], reqMAC) || !bytes.Equal(out[38:42], reqIP.To4()) {
			t.Fatalf("arp target = % x / % x, want requester % x / % x", out[32:38], out[38:42], reqMAC, reqIP.To4())
		}
	})

	t.Run("ignores non-gateway ARP", func(t *testing.T) {
		other := net.IPv4(10, 222, 0, 9)
		req := arpRequestFrame(reqMAC, reqIP, other)
		ret, _, err := objs.NatTap.Test(req)
		if err != nil {
			t.Fatalf("run nat_tap: %v", err)
		}
		if ret != tcActOK {
			t.Fatalf("verdict for non-gateway ARP = %d, want TC_ACT_OK(%d)", ret, tcActOK)
		}
	})
}
