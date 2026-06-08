//go:build linux

package main

import (
	"fmt"
	"net"

	"github.com/aarani/craftling-go/internal/runspec"
	"golang.org/x/sys/unix"
)

// setupNetwork brings the guest's MMDS interface up with its static
// link-local address. The kernel may already have done this from the
// ip= boot arg (CONFIG_IP_PNP), but that config option isn't universal
// across Firecracker kernels, so init configures the interface itself
// too. It is idempotent — re-assigning the same address the kernel
// already set is harmless — and best-effort: the caller logs a warning
// rather than aborting, because the MMDS fetch that follows will surface
// any real connectivity failure with a clearer error.
func setupNetwork() error {
	sock, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("socket: %w", err)
	}
	defer func() { _ = unix.Close(sock) }()

	if err := setIfaceAddr(sock, runspec.MMDSInterface, runspec.GuestIPv4, runspec.GuestNetmask); err != nil {
		return err
	}
	if err := setIfaceUp(sock, runspec.MMDSInterface); err != nil {
		return err
	}
	return nil
}

// setIfaceAddr assigns an IPv4 address and netmask to iface via the
// classic SIOCSIFADDR / SIOCSIFNETMASK ioctls.
func setIfaceAddr(sock int, iface, addr, mask string) error {
	if err := ioctlSetSockaddr(sock, unix.SIOCSIFADDR, iface, addr); err != nil {
		return fmt.Errorf("set addr %s on %s: %w", addr, iface, err)
	}
	if err := ioctlSetSockaddr(sock, unix.SIOCSIFNETMASK, iface, mask); err != nil {
		return fmt.Errorf("set netmask %s on %s: %w", mask, iface, err)
	}
	return nil
}

// setIfaceUp sets IFF_UP|IFF_RUNNING on iface.
func setIfaceUp(sock int, iface string) error {
	ifr, err := unix.NewIfreq(iface)
	if err != nil {
		return fmt.Errorf("ifreq %q: %w", iface, err)
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

// ioctlSetSockaddr issues an ifreq ioctl whose payload is a sockaddr_in
// carrying dotted-quad. x/sys/unix's Ifreq exposes SetInet4Addr, which
// packs exactly the AF_INET sockaddr the SIOCSIF{ADDR,NETMASK} ioctls
// expect.
func ioctlSetSockaddr(sock int, req uint, iface, dottedQuad string) error {
	ifr, err := unix.NewIfreq(iface)
	if err != nil {
		return fmt.Errorf("ifreq %q: %w", iface, err)
	}
	ip := net.ParseIP(dottedQuad).To4()
	if ip == nil {
		return fmt.Errorf("invalid IPv4 %q", dottedQuad)
	}
	if err := ifr.SetInet4Addr(ip); err != nil {
		return fmt.Errorf("set inet4 addr: %w", err)
	}
	return unix.IoctlIfreq(sock, req, ifr)
}
