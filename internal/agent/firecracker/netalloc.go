package firecracker

import (
	"encoding/binary"
	"fmt"
	"net"
	"sync"
)

// vmNet is the per-VM network identity assigned at provision time and shared
// between three consumers: the eBPF dataplane maps (vm_config / dnat_rules), the
// MMDS run spec handed to the guest (so the in-VM init agent can configure its
// interface), and the Firecracker network-interface config (which pins the
// guest MAC so inbound DNAT can address frames to it).
//
// VMIP is the guest's private /32 address; HostPort is the public host port
// DNAT'd to the in-VM service port (the P6 per-server host-port allocation).
type vmNet struct {
	VMIP       net.IP           // 4-byte IPv4 guest address
	VMMAC      net.HardwareAddr // 6-byte deterministic guest MAC
	HostPort   uint16           // public host port -> VM:vmServicePort
	GatewayIP  net.IP           // shared virtual gateway the guest routes through
	GatewayMAC net.HardwareAddr // gateway MAC for the guest's static neighbor
	PrefixLen  int              // guest address prefix length
}

// ipam hands out unique VM addresses and host ports for the NAT dataplane. It is
// an in-memory allocator: allocations live only as long as the agent process, so
// VMs are renumbered on a host restart (consistent with the rest of the driver,
// where the VM map is rebuilt rather than persisted). Safe for concurrent use.
type ipam struct {
	mu sync.Mutex

	network    *net.IPNet
	first      uint32 // first assignable host address (host byte order)
	last       uint32 // last assignable host address (inclusive)
	gatewayIP  net.IP
	gatewayMAC net.HardwareAddr

	usedIPs map[uint32]struct{}
	nextIP  uint32

	portMin   uint16
	portMax   uint16
	usedPorts map[uint16]struct{}
	nextPort  uint16
}

// newIPAM builds an allocator over subnet, reserving gatewayIP (which must fall
// inside subnet) and the network/broadcast addresses. Host ports are handed out
// from [portMin, portMax]. It validates that the usable range is non-empty.
func newIPAM(subnet *net.IPNet, gatewayIP net.IP, gatewayMAC net.HardwareAddr, portMin, portMax uint16) (*ipam, error) {
	v4 := subnet.IP.To4()
	if v4 == nil {
		return nil, fmt.Errorf("firecracker: VM subnet %s is not IPv4", subnet)
	}
	gw := gatewayIP.To4()
	if gw == nil {
		return nil, fmt.Errorf("firecracker: gateway %s is not IPv4", gatewayIP)
	}
	if !subnet.Contains(gw) {
		return nil, fmt.Errorf("firecracker: gateway %s not in subnet %s", gatewayIP, subnet)
	}
	if len(gatewayMAC) != 6 {
		return nil, fmt.Errorf("firecracker: gateway MAC %q is not 6 bytes", gatewayMAC)
	}
	if portMin == 0 || portMax < portMin {
		return nil, fmt.Errorf("firecracker: invalid host-port range %d-%d", portMin, portMax)
	}

	ones, bits := subnet.Mask.Size()
	if bits != 32 {
		return nil, fmt.Errorf("firecracker: VM subnet %s has a non-IPv4 mask", subnet)
	}
	netAddr := binary.BigEndian.Uint32(v4.Mask(subnet.Mask).To4())
	// /31 and /32 have no network/broadcast convention; otherwise skip both.
	first := netAddr
	last := netAddr | (0xffffffff >> uint(ones))
	if ones < 31 {
		first++ // skip network address
		last--  // skip broadcast address
	}
	if last < first {
		return nil, fmt.Errorf("firecracker: VM subnet %s has no assignable addresses", subnet)
	}

	ip := &ipam{
		network:    subnet,
		first:      first,
		last:       last,
		gatewayIP:  append(net.IP(nil), gw...),
		gatewayMAC: append(net.HardwareAddr(nil), gatewayMAC...),
		usedIPs:    map[uint32]struct{}{binary.BigEndian.Uint32(gw): {}},
		nextIP:     first,
		portMin:    portMin,
		portMax:    portMax,
		usedPorts:  map[uint16]struct{}{},
		nextPort:   portMin,
	}
	return ip, nil
}

// allocate reserves a free VM address and host port and returns the resulting
// vmNet (including the deterministic MAC). It errors when either pool is empty.
func (a *ipam) allocate() (vmNet, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	ipU32, err := a.takeIP()
	if err != nil {
		return vmNet{}, err
	}
	port, err := a.takePort()
	if err != nil {
		delete(a.usedIPs, ipU32)
		return vmNet{}, err
	}

	var ipb [4]byte
	binary.BigEndian.PutUint32(ipb[:], ipU32)
	ip := net.IP(append([]byte(nil), ipb[:]...))
	ones, _ := a.network.Mask.Size()
	return vmNet{
		VMIP:       ip,
		VMMAC:      deterministicMAC(ipU32),
		HostPort:   port,
		GatewayIP:  append(net.IP(nil), a.gatewayIP...),
		GatewayMAC: append(net.HardwareAddr(nil), a.gatewayMAC...),
		PrefixLen:  ones,
	}, nil
}

// release returns a vmNet's address and port to their pools. A zero/empty vmNet
// is ignored, so callers can release unconditionally on teardown.
func (a *ipam) release(n vmNet) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if v4 := n.VMIP.To4(); v4 != nil {
		delete(a.usedIPs, binary.BigEndian.Uint32(v4))
	}
	if n.HostPort != 0 {
		delete(a.usedPorts, n.HostPort)
	}
}

// takeIP returns the next free address, scanning from nextIP and wrapping once.
// Caller holds a.mu.
func (a *ipam) takeIP() (uint32, error) {
	span := a.last - a.first + 1
	for i := uint32(0); i < span; i++ {
		cand := a.nextIP
		a.nextIP++
		if a.nextIP > a.last {
			a.nextIP = a.first
		}
		if _, used := a.usedIPs[cand]; !used {
			a.usedIPs[cand] = struct{}{}
			return cand, nil
		}
	}
	return 0, fmt.Errorf("firecracker: VM address pool %s exhausted", a.network)
}

// takePort returns the next free host port, scanning from nextPort and wrapping
// once. Caller holds a.mu.
func (a *ipam) takePort() (uint16, error) {
	span := int(a.portMax) - int(a.portMin) + 1
	for i := 0; i < span; i++ {
		cand := a.nextPort
		if a.nextPort == a.portMax {
			a.nextPort = a.portMin
		} else {
			a.nextPort++
		}
		if _, used := a.usedPorts[cand]; !used {
			a.usedPorts[cand] = struct{}{}
			return cand, nil
		}
	}
	return 0, fmt.Errorf("firecracker: host-port pool %d-%d exhausted", a.portMin, a.portMax)
}

// deterministicMAC derives a stable, locally-administered unicast MAC from a
// VM's IPv4 address: 02:00 followed by the four address octets. The 0x02 high
// byte sets the locally-administered bit and clears the multicast bit, so the
// address never collides with a real vendor NIC and is reproducible from VMIP
// alone (the dataplane needs no separate MAC store).
func deterministicMAC(ipU32 uint32) net.HardwareAddr {
	mac := make(net.HardwareAddr, 6)
	mac[0] = 0x02
	mac[1] = 0x00
	binary.BigEndian.PutUint32(mac[2:], ipU32)
	return mac
}
