package firecracker

import (
	"net"
	"testing"
)

func mustCIDR(t *testing.T, s string) *net.IPNet {
	t.Helper()
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return n
}

func TestDeterministicMAC(t *testing.T) {
	// 10.222.0.5 -> 02:00:0a:de:00:05
	ip := net.IPv4(10, 222, 0, 5).To4()
	got := deterministicMAC(beUint32(ip))
	want := "02:00:0a:de:00:05"
	if got.String() != want {
		t.Fatalf("deterministicMAC = %s, want %s", got, want)
	}
}

// beUint32 mirrors the network-order conversion the allocator uses internally.
func beUint32(ip net.IP) uint32 {
	v := ip.To4()
	return uint32(v[0])<<24 | uint32(v[1])<<16 | uint32(v[2])<<8 | uint32(v[3])
}

func TestIPAMAllocateUniqueAndReserved(t *testing.T) {
	subnet := mustCIDR(t, "10.222.0.0/16")
	gw := net.IPv4(10, 222, 0, 1)
	mac, _ := net.ParseMAC("02:00:00:00:00:01")
	a, err := newIPAM(subnet, gw, mac, 30000, 30010)
	if err != nil {
		t.Fatalf("newIPAM: %v", err)
	}

	seen := map[string]bool{}
	for i := 0; i < 5; i++ {
		n, err := a.allocate()
		if err != nil {
			t.Fatalf("allocate %d: %v", i, err)
		}
		if n.VMIP.Equal(gw) {
			t.Fatalf("allocated the reserved gateway address %s", n.VMIP)
		}
		if !subnet.Contains(n.VMIP) {
			t.Fatalf("allocated %s outside subnet %s", n.VMIP, subnet)
		}
		if seen[n.VMIP.String()] {
			t.Fatalf("duplicate VM IP %s", n.VMIP)
		}
		seen[n.VMIP.String()] = true
		if n.HostPort < 30000 || n.HostPort > 30010 {
			t.Fatalf("host port %d out of range", n.HostPort)
		}
		if !n.GatewayIP.Equal(gw) {
			t.Fatalf("gateway = %s, want %s", n.GatewayIP, gw)
		}
		if n.PrefixLen != 16 {
			t.Fatalf("prefix = %d, want 16", n.PrefixLen)
		}
		// MAC must be deterministic from the IP.
		if want := deterministicMAC(beUint32(n.VMIP)); n.VMMAC.String() != want.String() {
			t.Fatalf("MAC %s does not match IP %s (want %s)", n.VMMAC, n.VMIP, want)
		}
	}
}

func TestIPAMReleaseReclaims(t *testing.T) {
	subnet := mustCIDR(t, "10.0.0.0/30") // .1/.2 usable after net/bcast skip
	gw := net.IPv4(10, 0, 0, 1)
	mac, _ := net.ParseMAC("02:00:00:00:00:01")
	a, err := newIPAM(subnet, gw, mac, 40000, 40000) // single port
	if err != nil {
		t.Fatalf("newIPAM: %v", err)
	}

	n1, err := a.allocate()
	if err != nil {
		t.Fatalf("allocate 1: %v", err)
	}
	// Only one usable address (.2; .1 is the gateway) and one port — the pool
	// is now exhausted.
	if _, err := a.allocate(); err == nil {
		t.Fatalf("expected exhaustion on second allocate")
	}
	a.release(n1)
	n2, err := a.allocate()
	if err != nil {
		t.Fatalf("allocate after release: %v", err)
	}
	if !n2.VMIP.Equal(n1.VMIP) || n2.HostPort != n1.HostPort {
		t.Fatalf("release did not reclaim: got %s:%d, freed %s:%d", n2.VMIP, n2.HostPort, n1.VMIP, n1.HostPort)
	}
}

func TestIPAMRejectsGatewayOutsideSubnet(t *testing.T) {
	subnet := mustCIDR(t, "10.222.0.0/16")
	mac, _ := net.ParseMAC("02:00:00:00:00:01")
	if _, err := newIPAM(subnet, net.IPv4(192, 168, 1, 1), mac, 30000, 40000); err == nil {
		t.Fatal("expected error for gateway outside subnet")
	}
}

func TestDataplaneConfigDefaults(t *testing.T) {
	c := Config{UplinkDevice: "eth0"}
	if !c.natEnabled() {
		t.Fatal("natEnabled should be true when UplinkDevice is set")
	}
	if err := c.validateDataplane(); err != nil {
		t.Fatalf("validateDataplane: %v", err)
	}
	dc, err := c.dataplaneConfig()
	if err != nil {
		t.Fatalf("dataplaneConfig: %v", err)
	}
	if dc.subnet.String() != DefaultVMSubnet {
		t.Fatalf("subnet = %s, want %s", dc.subnet, DefaultVMSubnet)
	}
	// GatewayIP defaults to the first usable host (network +1).
	if want := net.IPv4(10, 222, 0, 1).To4(); !dc.gatewayIP.Equal(want) {
		t.Fatalf("default gateway = %s, want %s", dc.gatewayIP, want)
	}
	if dc.portMin != DefaultHostPortMin || dc.portMax != DefaultHostPortMax {
		t.Fatalf("port range = %d-%d, want %d-%d", dc.portMin, dc.portMax, DefaultHostPortMin, DefaultHostPortMax)
	}
	if dc.gatewayMAC.String() != "02:00:00:00:00:01" {
		t.Fatalf("gateway MAC = %s", dc.gatewayMAC)
	}
}

func TestDataplaneConfigRejectsGatewayOutsideSubnet(t *testing.T) {
	c := Config{UplinkDevice: "eth0", VMSubnet: "10.222.0.0/16", GatewayIP: "10.99.0.1"}
	if _, err := c.dataplaneConfig(); err == nil {
		t.Fatal("expected error for gateway outside subnet")
	}
}

func TestConfigNatDisabledByDefault(t *testing.T) {
	c := Config{}
	if c.natEnabled() {
		t.Fatal("NAT should be disabled when UplinkDevice is empty")
	}
}
