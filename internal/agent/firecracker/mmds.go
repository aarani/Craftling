package firecracker

import (
	"context"
	"fmt"
	"strings"

	fcclient "github.com/aarani/craftling-go/internal/firecracker/client/operations"
	fcmodels "github.com/aarani/craftling-go/internal/firecracker/models"
	"github.com/aarani/craftling-go/internal/runspec"
)

// MMDS (microVM Metadata Service) is how the host hands the in-VM init
// agent its run spec without a shared filesystem. Firecracker exposes a
// mutable JSON store at a link-local address; the device model
// intercepts ARP and TCP destined for that address on a designated
// network interface and answers directly, so the data never traverses
// the host TAP. The guest only needs an interface up on the link-local
// subnet — set up by both the ip= boot arg and the init agent — and a
// plain HTTP GET (see internal/runspec.FetchFromMMDS).

// mmdsIfaceID is the Firecracker network-interface id the MMDS binds
// to — the guest's eth0. Addressing constants live in the runspec
// package so host and guest can't drift.
const mmdsIfaceID = runspec.MMDSInterface

// tapNameFor derives a deterministic host TAP name for a VM id. Linux
// caps interface names at 15 bytes (IFNAMSIZ-1), so we take a "fc"
// prefix plus the first 13 alphanumerics of the id (its UUID), dropping
// the "vm-" prefix and dashes.
func tapNameFor(vmID string) string {
	var b strings.Builder
	b.WriteString("fc")
	for i := 0; i < len(vmID) && b.Len() < 15; i++ {
		c := vmID[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') {
			b.WriteByte(c)
		}
	}
	return b.String()
}

// kernelIPArg is the kernel command-line directive that brings the
// guest's eth0 up with a static link-local address at boot, before init
// runs. Format: ip=<client>::<gw>:<mask>:<host>:<iface>:<autoconf>.
// We leave the hostname empty and disable autoconf.
func kernelIPArg() string {
	return fmt.Sprintf("ip=%s::%s:%s::%s:off",
		runspec.GuestIPv4, runspec.GuestGatewayIPv4, runspec.GuestNetmask, mmdsIfaceID)
}

// effectiveBootArgs returns the machine's boot args, appending the MMDS
// network directive when this VM publishes a run spec via MMDS.
func (m *machine) effectiveBootArgs() string {
	if m.runSpec == nil {
		return m.bootArgs
	}
	ip := kernelIPArg()
	if strings.Contains(m.bootArgs, "ip=") {
		// Operator already pinned networking; don't fight them.
		return m.bootArgs
	}
	if m.bootArgs == "" {
		return ip
	}
	return m.bootArgs + " " + ip
}

// configureMMDS attaches the MMDS network interface, points MMDS at it,
// and publishes the run spec. It runs against a pre-boot Firecracker
// (after the root drive is set, before InstanceStart) and is a no-op
// when this VM has no run spec to publish.
func (m *machine) configureMMDS(ctx context.Context) error {
	if m.runSpec == nil {
		return nil
	}

	// The host TAP must exist before Firecracker opens it. MMDS does not
	// need it routed or NATed — the device model answers MMDS traffic
	// itself — so a bare, up TAP is enough. The same TAP carries the VM's
	// real (NAT'd) traffic: MMDS is intercepted by the device model, every
	// other packet reaches the host TAP where nat_tap translates it.
	if err := createTAP(m.tapName); err != nil {
		return fmt.Errorf("create tap %q: %w", m.tapName, err)
	}

	ifaceID := mmdsIfaceID
	host := m.tapName
	nic := &fcmodels.NetworkInterface{
		IfaceID:     &ifaceID,
		HostDevName: &host,
	}
	// Pin the guest MAC to the deterministic dataplane MAC so inbound DNAT can
	// address frames to the VM without learning its MAC.
	if m.dp != nil && m.net.VMMAC != nil {
		nic.GuestMac = m.net.VMMAC.String()
	}
	if _, err := m.api.PutGuestNetworkInterfaceByID(fcclient.NewPutGuestNetworkInterfaceByIDParamsWithContext(ctx).
		WithIfaceID(ifaceID).
		WithBody(nic)); err != nil {
		return fmt.Errorf("network interface: %w", err)
	}

	// Attach the NAT dataplane to this TAP and publish the VM's maps now that
	// the device exists. The guest applies its address from the runspec Net
	// block once it boots and fetches MMDS.
	if m.dp != nil && m.net.VMMAC != nil {
		if err := m.dp.publishVM(m.tapName, m.net, m.servicePort); err != nil {
			return fmt.Errorf("nat dataplane: %w", err)
		}
	}

	version := fcmodels.MmdsConfigVersionV2
	ipv4 := runspec.MMDSIPv4
	if _, err := m.api.PutMmdsConfig(fcclient.NewPutMmdsConfigParamsWithContext(ctx).
		WithBody(&fcmodels.MmdsConfig{
			Version:           &version,
			IPv4Address:       &ipv4,
			NetworkInterfaces: []string{ifaceID},
		})); err != nil {
		return fmt.Errorf("mmds config: %w", err)
	}

	data, err := m.runSpec.MMDSData()
	if err != nil {
		return fmt.Errorf("build mmds data: %w", err)
	}
	if _, err := m.api.PutMmds(fcclient.NewPutMmdsParamsWithContext(ctx).
		WithBody(data)); err != nil {
		return fmt.Errorf("publish mmds data: %w", err)
	}
	return nil
}
