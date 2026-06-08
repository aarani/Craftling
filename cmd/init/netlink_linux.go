//go:build linux

package main

import (
	"encoding/binary"
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// This file is a tiny hand-rolled rtnetlink client: just enough to add a
// secondary address, a permanent neighbor, and a default route from inside the
// guest. The init binary ships in every rootfs and stays dependency-light, so
// we avoid pulling in a full netlink library and don't assume iproute2 exists
// in the (often minimal) image.

// nlAlign rounds up to the 4-byte netlink alignment.
func nlAlign(n int) int { return (n + unix.NLA_ALIGNTO - 1) &^ (unix.NLA_ALIGNTO - 1) }

// nlAttr encodes one rtattr (TLV), padded to alignment.
func nlAttr(typ uint16, data []byte) []byte {
	l := unix.SizeofRtAttr + len(data)
	b := make([]byte, nlAlign(l))
	binary.LittleEndian.PutUint16(b[0:2], uint16(l))
	binary.LittleEndian.PutUint16(b[2:4], typ)
	copy(b[unix.SizeofRtAttr:], data)
	return b
}

// nlExec sends a single rtnetlink request (with NLM_F_REQUEST|NLM_F_ACK and the
// caller's extra flags) and waits for the ACK, returning the kernel's error if
// any. body is the family-specific header followed by encoded attributes.
func nlExec(msgType uint16, extraFlags uint16, body []byte) error {
	s, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_ROUTE)
	if err != nil {
		return fmt.Errorf("netlink socket: %w", err)
	}
	defer func() { _ = unix.Close(s) }()
	if err := unix.Bind(s, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return fmt.Errorf("netlink bind: %w", err)
	}

	const hdrLen = unix.SizeofNlMsghdr // 16
	total := hdrLen + len(body)
	msg := make([]byte, nlAlign(total))
	binary.LittleEndian.PutUint32(msg[0:4], uint32(total))
	binary.LittleEndian.PutUint16(msg[4:6], msgType)
	binary.LittleEndian.PutUint16(msg[6:8], unix.NLM_F_REQUEST|unix.NLM_F_ACK|extraFlags)
	binary.LittleEndian.PutUint32(msg[8:12], 1)  // seq
	binary.LittleEndian.PutUint32(msg[12:16], 0) // pid: kernel assigns
	copy(msg[hdrLen:], body)

	if err := unix.Sendto(s, msg, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return fmt.Errorf("netlink send: %w", err)
	}

	resp := make([]byte, 4096)
	n, _, err := unix.Recvfrom(s, resp, 0)
	if err != nil {
		return fmt.Errorf("netlink recv: %w", err)
	}
	if n < unix.SizeofNlMsghdr {
		return fmt.Errorf("netlink: short reply (%d bytes)", n)
	}
	mtype := binary.LittleEndian.Uint16(resp[4:6])
	if mtype == unix.NLMSG_ERROR {
		// struct nlmsgerr { int error; struct nlmsghdr msg; } follows the hdr.
		if n < unix.SizeofNlMsghdr+4 {
			return fmt.Errorf("netlink: truncated error reply")
		}
		errno := int32(binary.LittleEndian.Uint32(resp[unix.SizeofNlMsghdr : unix.SizeofNlMsghdr+4]))
		if errno == 0 {
			return nil // ACK
		}
		return fmt.Errorf("netlink request failed: %w", unix.Errno(-errno))
	}
	return nil
}

// addInterfaceAddr adds ip/prefixLen to ifindex as a secondary address (the
// link-local MMDS address stays). NLM_F_REPLACE makes it idempotent.
func addInterfaceAddr(ifindex int, ip net.IP, prefixLen int) error {
	v4 := ip.To4()
	if v4 == nil {
		return fmt.Errorf("address %v is not IPv4", ip)
	}
	// struct ifaddrmsg { u8 family; u8 prefixlen; u8 flags; u8 scope; u32 index }
	hdr := make([]byte, 8)
	hdr[0] = unix.AF_INET
	hdr[1] = uint8(prefixLen)
	hdr[2] = 0                      // flags
	hdr[3] = unix.RT_SCOPE_UNIVERSE // global scope
	binary.LittleEndian.PutUint32(hdr[4:8], uint32(ifindex))

	body := append([]byte(nil), hdr...)
	body = append(body, nlAttr(unix.IFA_LOCAL, v4)...)
	body = append(body, nlAttr(unix.IFA_ADDRESS, v4)...)
	return nlExec(unix.RTM_NEWADDR, unix.NLM_F_CREATE|unix.NLM_F_REPLACE, body)
}

// addNeighbor installs a permanent ip -> mac neighbor on ifindex, so the guest
// never ARPs for the (host-interfaceless) gateway.
func addNeighbor(ifindex int, ip net.IP, mac net.HardwareAddr) error {
	v4 := ip.To4()
	if v4 == nil {
		return fmt.Errorf("neighbor %v is not IPv4", ip)
	}
	if len(mac) != 6 {
		return fmt.Errorf("neighbor MAC %v is not 48-bit", mac)
	}
	// struct ndmsg { u8 family; u8 pad1; u16 pad2; s32 ifindex; u16 state; u8 flags; u8 type }
	hdr := make([]byte, 12)
	hdr[0] = unix.AF_INET
	binary.LittleEndian.PutUint32(hdr[4:8], uint32(ifindex))
	binary.LittleEndian.PutUint16(hdr[8:10], unix.NUD_PERMANENT)

	body := append([]byte(nil), hdr...)
	body = append(body, nlAttr(unix.NDA_DST, v4)...)
	body = append(body, nlAttr(unix.NDA_LLADDR, mac)...)
	return nlExec(unix.RTM_NEWNEIGH, unix.NLM_F_CREATE|unix.NLM_F_REPLACE, body)
}

// addDefaultRoute adds a default route via gw out ifindex.
func addDefaultRoute(ifindex int, gw net.IP) error {
	v4 := gw.To4()
	if v4 == nil {
		return fmt.Errorf("gateway %v is not IPv4", gw)
	}
	// struct rtmsg { u8 family; u8 dst_len; u8 src_len; u8 tos; u8 table;
	//                u8 protocol; u8 scope; u8 type; u32 flags }
	hdr := make([]byte, 12)
	hdr[0] = unix.AF_INET
	hdr[1] = 0 // dst_len 0 => default route
	hdr[4] = unix.RT_TABLE_MAIN
	hdr[5] = unix.RTPROT_BOOT
	hdr[6] = unix.RT_SCOPE_UNIVERSE
	hdr[7] = unix.RTN_UNICAST

	oif := make([]byte, 4)
	binary.LittleEndian.PutUint32(oif, uint32(ifindex))

	body := append([]byte(nil), hdr...)
	body = append(body, nlAttr(unix.RTA_GATEWAY, v4)...)
	body = append(body, nlAttr(unix.RTA_OIF, oif)...)
	return nlExec(unix.RTM_NEWROUTE, unix.NLM_F_CREATE|unix.NLM_F_REPLACE, body)
}
