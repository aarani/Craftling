//go:build linux

package firecracker

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"sync"

	"github.com/aarani/craftling-go/internal/agent/firecracker/bpf"
	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
	"github.com/cilium/ebpf/rlimit"
	"golang.org/x/sys/unix"
)

// NatFlowEvent is one translation observed by the NAT dataplane, drained from
// the events ringbuf. Addresses are 4-byte IPv4 (network order, ready for
// net.IP); ports are host byte order.
type NatFlowEvent struct {
	VMIP    net.IP
	OrigSrc net.IP
	OrigDst net.IP
	NewSrc  net.IP
	NewDst  net.IP
	OrigSP  uint16
	OrigDP  uint16
	NewSP   uint16
	NewDP   uint16
	Length  uint16
	Proto   uint8
	Dir     uint8 // NatDir* below
	Verdict uint8 // 0 forwarded, 1 dropped
}

// Flow directions, mirroring EV_* in nat.c.
const (
	NatDirEgress      uint8 = 0
	NatDirEgressReply uint8 = 1
	NatDirInbound     uint8 = 2
	NatDirInboundRep  uint8 = 3
	NatDirDeny        uint8 = 4
)

// OnNatFlowEvent, if set, is called for every dataplane flow event. It runs on
// the dataplane's drain goroutine, so it must be cheap and concurrency-safe.
var OnNatFlowEvent func(NatFlowEvent)

// ---- map value layouts (mirror the structs in nat.c) -----------------------
//
// Network-order address/port fields are byte arrays so binary marshaling
// reproduces the on-wire bytes verbatim regardless of host endianness; purely
// host-side numeric fields (ifindex, prefix length, port comparisons) are
// native integers since the kernel reads them on the same host.

type natGlobalConfig struct {
	HostIP        [4]byte
	UplinkIfindex uint32
	GwIP          [4]byte
	GwMAC         [6]byte
	DefEgress     uint8
	DefIngress    uint8
}

type natVMEntry struct {
	VMIP       [4]byte
	TapIfindex uint32
	VMMAC      [6]byte
	_          [2]byte
}

type natDNATKey struct {
	HostIP   [4]byte
	HostPort [2]byte // network order
	Proto    uint8
	_        uint8
}

type natDNATVal struct {
	VMIP       [4]byte
	TapIfindex uint32
	VMPort     [2]byte // network order
	VMMAC      [6]byte
}

type natStats struct {
	RxPkts, RxBytes, TxPkts, TxBytes, Drops, Conns uint64
}

// natDataplane is the single shared eBPF collection: programs attached to the
// uplink (once) and every TAP, plus the maps userspace populates per VM.
type natDataplane struct {
	objs   bpf.NatObjects
	uplink link.Link
	hostIP net.IP

	mu       sync.Mutex
	tapLinks map[string]link.Link // tap name -> nat_tap TCX link

	reader *ringbuf.Reader
	done   chan struct{}
}

// newDataplane loads the shared collection, writes global config, and attaches
// nat_uplink to the uplink. nat_tap is attached per-TAP later via publishVM.
// Requires CAP_BPF + CAP_NET_ADMIN, a >= 6.6 kernel, and a loaded nf_conntrack.
func newDataplane(dc dataplaneConfig) (*natDataplane, error) {
	if err := ensureConntrack(); err != nil {
		return nil, err
	}

	iface, err := net.InterfaceByName(dc.uplink)
	if err != nil {
		return nil, fmt.Errorf("firecracker: uplink %q: %w", dc.uplink, err)
	}
	hostIP, err := primaryIPv4(iface)
	if err != nil {
		return nil, err
	}

	// Harmless on >=5.11 where memcg accounting replaces the rlimit.
	_ = rlimit.RemoveMemlock()

	dp := &natDataplane{
		hostIP:   hostIP,
		tapLinks: map[string]link.Link{},
		done:     make(chan struct{}),
	}
	if err := bpf.LoadNatObjects(&dp.objs, nil); err != nil {
		return nil, fmt.Errorf("firecracker: load nat objects (kernel >=6.6 + nf_conntrack kfuncs?): %w", err)
	}

	gc := natGlobalConfig{
		UplinkIfindex: uint32(iface.Index),
		GwMAC:         macArray(dc.gatewayMAC),
		DefEgress:     1, // default-allow egress; deny entries are a blocklist
		DefIngress:    1, // inbound is already gated by the published-port map
	}
	copy(gc.HostIP[:], hostIP.To4())
	copy(gc.GwIP[:], dc.gatewayIP.To4())
	if err := dp.objs.GlobalConfigMap.Put(uint32(0), &gc); err != nil {
		dp.objs.Close()
		return nil, fmt.Errorf("firecracker: write global config: %w", err)
	}

	dp.uplink, err = link.AttachTCX(link.TCXOptions{
		Interface: iface.Index,
		Program:   dp.objs.NatUplink,
		Attach:    ebpf.AttachTCXIngress,
	})
	if err != nil {
		dp.objs.Close()
		return nil, fmt.Errorf("firecracker: attach nat_uplink to %s: %w", dc.uplink, err)
	}

	dp.reader, err = ringbuf.NewReader(dp.objs.Events)
	if err != nil {
		_ = dp.uplink.Close()
		dp.objs.Close()
		return nil, fmt.Errorf("firecracker: open events ringbuf: %w", err)
	}
	go dp.drain()
	return dp, nil
}

// publishVM attaches nat_tap to the VM's TAP, registers the VM in vm_config
// (for reply redirection), and installs the published-port DNAT rule for the
// in-VM service port. Idempotent per TAP name.
func (dp *natDataplane) publishVM(tapName string, n vmNet, vmServicePort uint16) error {
	iface, err := net.InterfaceByName(tapName)
	if err != nil {
		return fmt.Errorf("firecracker: tap %q: %w", tapName, err)
	}

	dp.mu.Lock()
	defer dp.mu.Unlock()
	if _, ok := dp.tapLinks[tapName]; ok {
		return nil
	}

	l, err := link.AttachTCX(link.TCXOptions{
		Interface: iface.Index,
		Program:   dp.objs.NatTap,
		Attach:    ebpf.AttachTCXIngress,
	})
	if err != nil {
		return fmt.Errorf("firecracker: attach nat_tap to %s: %w", tapName, err)
	}

	var key [4]byte
	copy(key[:], n.VMIP.To4())
	vm := natVMEntry{TapIfindex: uint32(iface.Index), VMMAC: macArray(n.VMMAC)}
	copy(vm.VMIP[:], n.VMIP.To4())
	if err := dp.objs.VmConfig.Put(&key, &vm); err != nil {
		_ = l.Close()
		return fmt.Errorf("firecracker: write vm_config: %w", err)
	}

	// Publish host_ip:host_port (TCP) -> VM:vmServicePort. Minecraft is TCP.
	dk := natDNATKey{HostPort: be16(n.HostPort), Proto: unix.IPPROTO_TCP}
	copy(dk.HostIP[:], dp.hostIP.To4())
	dv := natDNATVal{TapIfindex: uint32(iface.Index), VMPort: be16(vmServicePort), VMMAC: macArray(n.VMMAC)}
	copy(dv.VMIP[:], n.VMIP.To4())
	if err := dp.objs.DnatRules.Put(&dk, &dv); err != nil {
		_ = dp.objs.VmConfig.Delete(&key)
		_ = l.Close()
		return fmt.Errorf("firecracker: write dnat rule: %w", err)
	}

	dp.tapLinks[tapName] = l
	return nil
}

// withdrawVM detaches nat_tap and removes the VM's map entries. Safe for a TAP
// that was never published.
func (dp *natDataplane) withdrawVM(tapName string, n vmNet) {
	dp.mu.Lock()
	l, ok := dp.tapLinks[tapName]
	if ok {
		delete(dp.tapLinks, tapName)
	}
	dp.mu.Unlock()
	if ok {
		_ = l.Close()
	}

	if v4 := n.VMIP.To4(); v4 != nil {
		var key [4]byte
		copy(key[:], v4)
		_ = dp.objs.VmConfig.Delete(&key)
	}
	dk := natDNATKey{HostPort: be16(n.HostPort), Proto: unix.IPPROTO_TCP}
	copy(dk.HostIP[:], dp.hostIP.To4())
	_ = dp.objs.DnatRules.Delete(&dk)
}

// vmStats sums the per-CPU counters for a VM. ok is false if none are recorded.
func (dp *natDataplane) vmStats(vmIP net.IP) (natStats, bool) {
	v4 := vmIP.To4()
	if v4 == nil {
		return natStats{}, false
	}
	var key [4]byte
	copy(key[:], v4)
	per := make([]natStats, possibleCPUs())
	if err := dp.objs.Stats.Lookup(&key, &per); err != nil {
		return natStats{}, false
	}
	var sum natStats
	for _, s := range per {
		sum.RxPkts += s.RxPkts
		sum.RxBytes += s.RxBytes
		sum.TxPkts += s.TxPkts
		sum.TxBytes += s.TxBytes
		sum.Drops += s.Drops
		sum.Conns += s.Conns
	}
	return sum, true
}

// Close detaches everything and frees the collection.
func (dp *natDataplane) Close() {
	if dp == nil {
		return
	}
	if dp.reader != nil {
		_ = dp.reader.Close()
		<-dp.done
	}
	dp.mu.Lock()
	for _, l := range dp.tapLinks {
		_ = l.Close()
	}
	dp.tapLinks = map[string]link.Link{}
	dp.mu.Unlock()
	if dp.uplink != nil {
		_ = dp.uplink.Close()
	}
	dp.objs.Close()
}

func (dp *natDataplane) drain() {
	defer close(dp.done)
	for {
		rec, err := dp.reader.Read()
		if err != nil {
			if errors.Is(err, ringbuf.ErrClosed) {
				return
			}
			continue
		}
		if OnNatFlowEvent == nil {
			continue
		}
		if ev, ok := decodeNatEvent(rec.RawSample); ok {
			OnNatFlowEvent(ev)
		}
	}
}

// decodeNatEvent parses a raw ringbuf sample (struct nat_event in nat.c). The
// address fields are network order on the wire; ports are network order and
// swapped to host order here.
func decodeNatEvent(raw []byte) (NatFlowEvent, bool) {
	if len(raw) < 33 {
		return NatFlowEvent{}, false
	}
	be := binary.BigEndian
	ip := func(off int) net.IP { return net.IP(append([]byte(nil), raw[off:off+4]...)) }
	return NatFlowEvent{
		OrigSrc: ip(0),
		OrigDst: ip(4),
		NewSrc:  ip(8),
		NewDst:  ip(12),
		VMIP:    ip(16),
		OrigSP:  be.Uint16(raw[20:22]),
		OrigDP:  be.Uint16(raw[22:24]),
		NewSP:   be.Uint16(raw[24:26]),
		NewDP:   be.Uint16(raw[26:28]),
		Length:  be.Uint16(raw[28:30]),
		Proto:   raw[30],
		Dir:     raw[31],
		Verdict: raw[32],
	}, true
}

// ensureConntrack loads the conntrack/NAT modules (a module + sysctl
// dependency, not iptables rules) so the bpf_ct_* kfuncs have a conntrack table
// to work on. nf_nat matters specifically: bpf_ct_set_nat_info — the source-port
// allocator and DNAT binding — is registered by the nf_nat module, so without it
// loaded the program fails to verify even when nf_conntrack is up. modprobe
// failing is not fatal here (the modules may be builtin); LoadNatObjects is the
// real gate and reports clearly if the kfuncs are genuinely unavailable.
func ensureConntrack() error {
	for _, mod := range []string{"nf_conntrack", "nf_nat"} {
		// Ignore errors: builtin modules have nothing to load, and a missing
		// module surfaces as a kfunc-resolution failure at load time.
		_ = exec.Command("modprobe", mod).Run()
	}
	return nil
}

// primaryIPv4 returns the first non-loopback IPv4 address on iface.
func primaryIPv4(iface *net.Interface) (net.IP, error) {
	addrs, err := iface.Addrs()
	if err != nil {
		return nil, fmt.Errorf("firecracker: uplink %s addrs: %w", iface.Name, err)
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		if v4 := ipnet.IP.To4(); v4 != nil && !v4.IsLoopback() {
			return v4, nil
		}
	}
	return nil, fmt.Errorf("firecracker: uplink %s has no IPv4 address", iface.Name)
}

func macArray(m net.HardwareAddr) [6]byte {
	var a [6]byte
	copy(a[:], m)
	return a
}

func be16(v uint16) [2]byte {
	var b [2]byte
	binary.BigEndian.PutUint16(b[:], v)
	return b
}

func possibleCPUs() int {
	n, err := ebpf.PossibleCPU()
	if err != nil || n <= 0 {
		return 1
	}
	return n
}
