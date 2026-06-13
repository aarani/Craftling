// Package firecracker is the real agent Runtime (P4): it boots each game server
// as a Firecracker microVM instead of simulating it like agent.FakeRuntime.
//
// It drives Firecracker through the in-repo generated REST client
// (internal/firecracker), spoken over the per-VM API Unix socket, and manages
// the Firecracker process lifecycle directly. When WorldPersistence is enabled
// (P5a) a runspec VM also gets a per-server writable world disk attached as a
// second drive, which the in-VM init overlays onto WorkingDir so the world
// survives a stop/start; backup-to-store and cross-host reschedule build on
// that in P5b/P5c.
//
// This package only runs on a Linux host with /dev/kvm; its integration test is
// gated behind the `kvm` build tag and kept out of the default CI lane.
package firecracker

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/aarani/craftling-go/internal/image"
	"github.com/aarani/craftling-go/internal/runspec"
	"github.com/aarani/craftling-go/internal/storage"
	"go.uber.org/zap"
)

// Config configures the Firecracker Runtime. Paths point at host artifacts
// (kernel, per-version rootfs images) provided out of band on the agent host.
type Config struct {
	// BinaryPath is the firecracker executable. Defaults to "firecracker" on PATH.
	BinaryPath string
	// KernelPath is the uncompressed kernel (vmlinux) all VMs boot.
	KernelPath string
	// ImageDir holds per-version base rootfs images named "minecraft-<version>.ext4".
	// Optional when ImageStore is set and every spec carries an OCI image.
	ImageDir string
	// DefaultImage is the rootfs filename (within ImageDir) used when a spec's
	// version has no dedicated image. Empty means an unknown version is rejected.
	DefaultImage string
	// ImageStore, when non-nil, builds (or reuses) a read-only squashfs rootfs
	// from the OCI image a spec names, injecting the in-VM init agent. A spec
	// with a non-empty Image boots from it instead of the legacy ext4 base; the
	// image's run spec is published over MMDS. Nil disables the OCI path.
	ImageStore *image.Store
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

	// WorldPersistence enables the per-server writable world disk + guest
	// overlay (P5a). It applies only to runspec/init VMs — the legacy ext4
	// image is already a fully writable per-VM copy. When false (the
	// default) VMs boot with no data disk and a write to WorkingDir lives
	// in tmpfs (lost on stop). Enabling it requires mkfs.ext4 on the host
	// and CONFIG_OVERLAY_FS + CONFIG_EXT4_FS in the guest kernel.
	WorldPersistence bool
	// DataDir is where per-server world disks live, deliberately separate
	// from the per-VM WorkDir (which Deprovision wipes) so a world can
	// survive stop/start and outlive a single VM instance. Defaults to a
	// "worlds" dir under WorkDir. Only used when WorldPersistence is set.
	DataDir string
	// WorldDiskMB is the size of a freshly created world disk. The ext4 is
	// created sparse, so this is a ceiling, not an upfront allocation.
	// Default DefaultWorldDiskMB.
	WorldDiskMB int
	// MkfsExt4Path is the mkfs.ext4 executable used to format a new world
	// disk. Defaults to "mkfs.ext4" resolved on PATH.
	MkfsExt4Path string
	// WorldStore, when non-nil, is the durable off-host home of world
	// snapshots (P5b): Provision restores a server's world from it before
	// boot, Stop snapshots the disk into it, and Deprovision deletes it.
	// Nil keeps worlds local-only (they survive stop/start on this host but
	// not delete or reschedule). Only consulted when WorldPersistence is set.
	WorldStore storage.WorldStore

	// SnapshotInterval, when > 0, turns on periodic application-consistent
	// snapshots (P5c) of every running VM, bounding crash data-loss to one
	// interval. Requires a WorldStore. 0 disables the periodic sweep (a live
	// snapshot can still be taken on demand).
	SnapshotInterval time.Duration
	// RCONPort is the in-VM RCON port the guest flushes through before a live
	// snapshot. Default DefaultRCONPort.
	RCONPort int
	// RCONPassword authenticates to the in-VM RCON. Empty means snapshots
	// freeze the disk without an application flush (filesystem-consistent
	// only). Shared across the agent's servers for now.
	RCONPassword string
	// Logger is used for best-effort background work (the periodic snapshot
	// sweep). Nil is replaced with a no-op logger.
	Logger *zap.Logger
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

// World-persistence defaults (P5a).
const (
	// DefaultWorldDiskMB is the size of a freshly created world disk. It is
	// created sparse, so this caps growth rather than reserving the space.
	DefaultWorldDiskMB = 4096
	// defaultMkfsExt4 is the mkfs.ext4 executable resolved on PATH when
	// MkfsExt4Path is unset.
	defaultMkfsExt4 = "mkfs.ext4"
	// worldDriveID is the Firecracker drive id of the per-server world disk.
	worldDriveID = "world"
	// worldDevice is the guest block device the world disk surfaces as. The
	// root squashfs is /dev/vda; the world disk, attached second, is /dev/vdb.
	worldDevice = "/dev/vdb"
	// DefaultRCONPort is the in-VM RCON port flushed before a live snapshot.
	DefaultRCONPort = 25575
)

// DefaultBootArgs is a minimal serial-console boot line that mounts the rootfs
// read-write off the first virtio block device (the legacy ext4 path).
const DefaultBootArgs = "console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda rw"

// ociBootArgs boots an OCI squashfs rootfs: read-only root with the injected
// Go init agent as PID 1. effectiveBootArgs appends the MMDS ip= directive
// since these VMs always carry a run spec.
const ociBootArgs = "console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda ro init=" + runspec.InitPath

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
	// ImageDir backs the legacy per-version ext4 path; it is optional when an
	// ImageStore (OCI rootfs) is configured. At least one source must exist.
	if c.ImageDir == "" && c.ImageStore == nil {
		return errors.New("firecracker: one of ImageDir or ImageStore is required")
	}
	if c.ImageDir != "" {
		if fi, err := os.Stat(c.ImageDir); err != nil {
			return fmt.Errorf("firecracker: image dir: %w", err)
		} else if !fi.IsDir() {
			return fmt.Errorf("firecracker: image dir %q is not a directory", c.ImageDir)
		}
	}
	if c.natEnabled() {
		if err := c.validateDataplane(); err != nil {
			return err
		}
	}
	if c.persistEnabled() {
		if err := c.validatePersistence(); err != nil {
			return err
		}
	}
	return nil
}

// natEnabled reports whether the eBPF NAT dataplane should be wired up. It is
// gated on UplinkDevice so a host without a configured uplink keeps the legacy
// MMDS-only behaviour.
func (c *Config) natEnabled() bool { return c.UplinkDevice != "" }

// persistEnabled reports whether per-server world disks should be created and
// overlaid (P5a). Off by default so MMDS-only hosts are unchanged.
func (c *Config) persistEnabled() bool { return c.WorldPersistence }

// liveSnapshotEnabled reports whether VMs should get a vsock control device and
// a Quiesce runspec so the host can snapshot them while running (P5c). It needs
// both a world disk (to freeze) and a store (to snapshot into).
func (c *Config) liveSnapshotEnabled() bool {
	return c.persistEnabled() && c.WorldStore != nil
}

// validatePersistence fills world-disk defaults and checks that mkfs.ext4 is
// available, so a misconfigured host fails fast at startup rather than at the
// first Provision. It is only called when persistEnabled.
func (c *Config) validatePersistence() error {
	if c.DataDir == "" {
		c.DataDir = filepath.Join(c.WorkDir, "worlds")
	}
	if c.WorldDiskMB <= 0 {
		c.WorldDiskMB = DefaultWorldDiskMB
	}
	if c.MkfsExt4Path == "" {
		c.MkfsExt4Path = defaultMkfsExt4
	}
	if c.RCONPort == 0 {
		c.RCONPort = DefaultRCONPort
	}
	if c.Logger == nil {
		c.Logger = zap.NewNop()
	}
	if err := resolveExecutable(c.MkfsExt4Path); err != nil {
		return fmt.Errorf("firecracker: world persistence needs mkfs.ext4: %w", err)
	}
	if err := os.MkdirAll(c.DataDir, 0o750); err != nil {
		return fmt.Errorf("firecracker: world data dir: %w", err)
	}
	return nil
}

// resolveExecutable verifies a configured tool is runnable: an explicit path
// (containing a separator) must exist, a bare name must resolve on PATH.
func resolveExecutable(name string) error {
	if strings.ContainsRune(name, os.PathSeparator) {
		if _, err := os.Stat(name); err != nil {
			return err
		}
		return nil
	}
	if _, err := exec.LookPath(name); err != nil {
		return err
	}
	return nil
}

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
