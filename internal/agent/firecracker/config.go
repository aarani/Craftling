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
	// AdvertiseHost is the player-facing connect address VMs report (a stand-in
	// until P6 derives a real per-VM address from networking).
	AdvertiseHost string
	// BootArgs overrides the kernel command line. Empty uses DefaultBootArgs.
	BootArgs string
}

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
	return nil
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
