//go:build linux

package firecracker

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// createTAP creates (or, if it already exists, re-attaches to) a
// persistent TAP device named name and brings it up. Firecracker opens
// the device by name when the network interface is configured, so it
// must exist and be up beforehand. The device is made persistent
// (TUNSETPERSIST) so it survives this process closing the /dev/net/tun
// fd — Firecracker, not us, is its long-term user. Requires
// CAP_NET_ADMIN, which the Firecracker host has.
func createTAP(name string) error {
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open /dev/net/tun: %w", err)
	}
	defer func() { _ = unix.Close(fd) }()

	ifr, err := unix.NewIfreq(name)
	if err != nil {
		return fmt.Errorf("ifreq %q: %w", name, err)
	}
	// TAP (L2) without the 4-byte packet-info prefix.
	ifr.SetUint16(unix.IFF_TAP | unix.IFF_NO_PI)
	if err := unix.IoctlIfreq(fd, unix.TUNSETIFF, ifr); err != nil {
		return fmt.Errorf("TUNSETIFF: %w", err)
	}
	if err := unix.IoctlSetInt(fd, unix.TUNSETPERSIST, 1); err != nil {
		return fmt.Errorf("TUNSETPERSIST: %w", err)
	}
	if err := bringUp(name); err != nil {
		return err
	}
	// Optionally attach the tapfilter eBPF program (observe/drop a single
	// port). No-op unless CRAFTLING_TAP_FILTER_PORT is set.
	return maybeAttachTAPFilter(name)
}

// bringUp sets the IFF_UP flag on the named interface.
func bringUp(name string) error {
	sock, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("socket: %w", err)
	}
	defer func() { _ = unix.Close(sock) }()

	ifr, err := unix.NewIfreq(name)
	if err != nil {
		return fmt.Errorf("ifreq %q: %w", name, err)
	}
	if err := unix.IoctlIfreq(sock, unix.SIOCGIFFLAGS, ifr); err != nil {
		return fmt.Errorf("SIOCGIFFLAGS: %w", err)
	}
	ifr.SetUint16(ifr.Uint16() | unix.IFF_UP | unix.IFF_RUNNING)
	if err := unix.IoctlIfreq(sock, unix.SIOCSIFFLAGS, ifr); err != nil {
		return fmt.Errorf("SIOCSIFFLAGS: %w", err)
	}
	return nil
}

// deleteTAP removes a persistent TAP device. A missing device is not an
// error — teardown can race a crash that never created it.
func deleteTAP(name string) error {
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open /dev/net/tun: %w", err)
	}
	defer func() { _ = unix.Close(fd) }()

	ifr, err := unix.NewIfreq(name)
	if err != nil {
		return fmt.Errorf("ifreq %q: %w", name, err)
	}
	// Tear down any eBPF filter first so its TCX links and maps are released
	// before the device disappears.
	detachTAPFilter(name)

	ifr.SetUint16(unix.IFF_TAP | unix.IFF_NO_PI)
	if err := unix.IoctlIfreq(fd, unix.TUNSETIFF, ifr); err != nil {
		// ENODEV means it's already gone; treat as success.
		if err == unix.ENODEV {
			return nil
		}
		return fmt.Errorf("TUNSETIFF: %w", err)
	}
	// Clearing persistence makes the device disappear when the fd closes.
	if err := unix.IoctlSetInt(fd, unix.TUNSETPERSIST, 0); err != nil {
		return fmt.Errorf("TUNSETPERSIST off: %w", err)
	}
	return nil
}
