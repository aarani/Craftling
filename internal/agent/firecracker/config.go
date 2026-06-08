// Package firecracker is the real agent Runtime (P4): it boots each game server
// as a Firecracker microVM instead of simulating it like agent.FakeRuntime.
//
// It drives Firecracker through the in-repo generated REST client
// (internal/firecracker), spoken over the per-VM API Unix socket, and manages
// the Firecracker process lifecycle directly. Networking (P6) and world
// persistence (P5) are deliberately out of scope here: a VM boots from a
// per-version rootfs with the standard in-VM Minecraft port, and the driver
// reports the host's advertise address as the connect host until P6 wires real
// per-VM networking.
//
// This package only runs on a Linux host with /dev/kvm; its integration test is
// gated behind the `kvm` build tag and kept out of the default CI lane.
package firecracker

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
)

// Config configures the Firecracker Runtime. Paths point at host artifacts
// (kernel, per-version rootfs images) provided out of band on the agent host.
type Config struct {
	// BinaryPath is the firecracker executable. Defaults to "firecracker" on PATH.
	BinaryPath string
	// KernelPath is the uncompressed kernel (vmlinux) all VMs boot.
	KernelPath string
	// ImageDir holds per-version base rootfs images named "minecraft-<version>.ext4".
	ImageDir string
	// DefaultImage is the rootfs filename (within ImageDir) used when a spec's
	// version has no dedicated image. Empty means an unknown version is rejected.
	DefaultImage string
	// WorkDir is where per-VM working directories (sockets, writable rootfs,
	// logs) live. Defaults to a "craftling-fc" dir under the OS temp dir.
	WorkDir string
	// AdvertiseHost is the player-facing connect address VMs report. With the
	// eBPF NAT dataplane enabled (UplinkDevice set) this is the host's public
	// address; the per-server port is the IPAM-allocated host port (see vmNet).
	AdvertiseHost string
	// BootArgs overrides the kernel command line. Empty uses DefaultBootArgs.
	BootArgs string

	// UplinkDevice is the host NIC the NAT dataplane attaches to for egress
	// SNAT and inbound DNAT (e.g. "eth0", "ens5"). When empty the NAT dataplane
	// is disabled and VMs get MMDS-only networking as before — every other
	// dataplane field below is then ignored.
	UplinkDevice string
	// VMSubnet is the CIDR private VM addresses are drawn from. Default
	// DefaultVMSubnet.
	VMSubnet string
	// GatewayIP is the shared virtual gateway address VMs route through. It is
	// never assigned to a host interface (the dataplane redirects, it does not
	// route). Must fall inside VMSubnet. Empty defaults to the first usable host.
	GatewayIP string
	// GatewayMAC is the MAC the guest installs as a static neighbor for
	// GatewayIP. Default DefaultGatewayMAC.
	GatewayMAC string
	// HostPortMin/HostPortMax bound the public host-port pool DNAT'd to in-VM
	// services. Defaults DefaultHostPortMin/Max.
	HostPortMin uint16
	HostPortMax uint16
}

// Dataplane defaults. The VM subnet is a private RFC1918 block unlikely to
// clash with host or upstream networks; the gateway MAC is locally-administered
// (0x02 high byte) so it never collides with a real NIC.
const (
	DefaultVMSubnet    = "10.222.0.0/16"
	DefaultGatewayMAC  = "02:00:00:00:00:01"
	DefaultHostPortMin = 30000
	DefaultHostPortMax = 40000
)

// DefaultBootArgs is a minimal serial-console boot line that mounts the rootfs
// read-write off the first virtio block device.
const DefaultBootArgs = "console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda rw"

// defaultMinecraftPort is the in-VM Minecraft server port. Per-server host port
// allocation arrives in P6; until then every VM uses the standard port.
const defaultMinecraftPort = 25565

// validate fills defaults and checks that the required host artifacts exist.
func (c *Config) validate() error {
	if c.BinaryPath == "" {
		c.BinaryPath = "firecracker"
	}
	if c.WorkDir == "" {
		c.WorkDir = filepath.Join(os.TempDir(), "craftling-fc")
	}
	if c.AdvertiseHost == "" {
		c.AdvertiseHost = "127.0.0.1"
	}
	if c.BootArgs == "" {
		c.BootArgs = DefaultBootArgs
	}
	if c.KernelPath == "" {
		return errors.New("firecracker: KernelPath is required")
	}
	if _, err := os.Stat(c.KernelPath); err != nil {
		return fmt.Errorf("firecracker: kernel image: %w", err)
	}
	if c.ImageDir == "" {
		return errors.New("firecracker: ImageDir is required")
	}
	if fi, err := os.Stat(c.ImageDir); err != nil {
		return fmt.Errorf("firecracker: image dir: %w", err)
	} else if !fi.IsDir() {
		return fmt.Errorf("firecracker: image dir %q is not a directory", c.ImageDir)
	}
	if c.natEnabled() {
		if err := c.validateDataplane(); err != nil {
			return err
		}
	}
	return nil
}

// natEnabled reports whether the eBPF NAT dataplane should be wired up. It is
// gated on UplinkDevice so a host without a configured uplink keeps the legacy
// MMDS-only behaviour.
func (c *Config) natEnabled() bool { return c.UplinkDevice != "" }

// validateDataplane fills dataplane defaults and checks the addressing is
// self-consistent (subnet parses, gateway sits inside it, port range is sane).
// It is only called when natEnabled.
func (c *Config) validateDataplane() error {
	if c.VMSubnet == "" {
		c.VMSubnet = DefaultVMSubnet
	}
	if c.GatewayMAC == "" {
		c.GatewayMAC = DefaultGatewayMAC
	}
	if c.HostPortMin == 0 {
		c.HostPortMin = DefaultHostPortMin
	}
	if c.HostPortMax == 0 {
		c.HostPortMax = DefaultHostPortMax
	}
	// dataplaneConfig does the cross-field validation (parse + containment).
	_, err := c.dataplaneConfig()
	return err
}

// dataplaneConfig parses the addressing fields into a ready-to-use form,
// defaulting GatewayIP to the first usable host of VMSubnet when unset.
func (c *Config) dataplaneConfig() (dataplaneConfig, error) {
	_, subnet, err := net.ParseCIDR(c.VMSubnet)
	if err != nil {
		return dataplaneConfig{}, fmt.Errorf("firecracker: VMSubnet %q: %w", c.VMSubnet, err)
	}
	gwMAC, err := net.ParseMAC(c.GatewayMAC)
	if err != nil {
		return dataplaneConfig{}, fmt.Errorf("firecracker: GatewayMAC %q: %w", c.GatewayMAC, err)
	}
	if len(gwMAC) != 6 {
		return dataplaneConfig{}, fmt.Errorf("firecracker: GatewayMAC %q is not 48-bit", c.GatewayMAC)
	}

	gwIP := net.ParseIP(c.GatewayIP).To4()
	if c.GatewayIP == "" {
		// First usable host = network address + 1.
		host := append(net.IP(nil), subnet.IP.To4()...)
		host[3]++
		gwIP = host
	}
	if gwIP == nil {
		return dataplaneConfig{}, fmt.Errorf("firecracker: GatewayIP %q is not IPv4", c.GatewayIP)
	}
	if !subnet.Contains(gwIP) {
		return dataplaneConfig{}, fmt.Errorf("firecracker: GatewayIP %s not in VMSubnet %s", gwIP, subnet)
	}
	if c.HostPortMin == 0 || c.HostPortMax < c.HostPortMin {
		return dataplaneConfig{}, fmt.Errorf("firecracker: invalid host-port range %d-%d", c.HostPortMin, c.HostPortMax)
	}
	return dataplaneConfig{
		uplink:     c.UplinkDevice,
		subnet:     subnet,
		gatewayIP:  gwIP,
		gatewayMAC: gwMAC,
		portMin:    c.HostPortMin,
		portMax:    c.HostPortMax,
	}, nil
}

// dataplaneConfig is the validated, parsed form of the NAT addressing knobs.
type dataplaneConfig struct {
	uplink     string
	subnet     *net.IPNet
	gatewayIP  net.IP
	gatewayMAC net.HardwareAddr
	portMin    uint16
	portMax    uint16
}

// imageFor resolves the base rootfs image path for a Minecraft version,
// falling back to DefaultImage. It returns an error if neither exists, so an
// unsupported version fails the provision rather than booting a wrong world.
func (c *Config) imageFor(version string) (string, error) {
	if version != "" {
		p := filepath.Join(c.ImageDir, "minecraft-"+version+".ext4")
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	if c.DefaultImage != "" {
		p := filepath.Join(c.ImageDir, c.DefaultImage)
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
		return "", fmt.Errorf("firecracker: default image %q not found in %s", c.DefaultImage, c.ImageDir)
	}
	return "", fmt.Errorf("firecracker: no rootfs image for version %q in %s", version, c.ImageDir)
}
