package firecracker

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/aarani/craftling-go/internal/runspec"
)

// Host side of application-consistent snapshots (P5c). The guest runs a control
// server on AF_VSOCK (see cmd/init/vsock_linux.go); Firecracker exposes the
// guest's vsock to the host as a Unix socket (the VM's UDS). To reach the guest
// listener the host connects to that UDS and sends "CONNECT <port>\n"; on an
// "OK" line the stream is wired straight to the guest. We then drive a
// PREPARE → (snapshot) → RESUME exchange so the disk is frozen exactly while we
// read it.

// guestCID is the vsock context id assigned to every VM. Each VM has its own
// UDS, which is what actually disambiguates them on the host, so a fixed CID
// (the minimum Firecracker allows) is fine.
const guestCID = 3

// vsockDialTimeout bounds the UDS connect + handshake; snapshotDeadline bounds
// the whole frozen window (handshake + disk read + thaw) so a hung guest can't
// keep a server's disk frozen forever.
const (
	vsockDialTimeout = 10 * time.Second
	snapshotDeadline = 5 * time.Minute
)

// snapshotRunning takes an application-consistent snapshot of a running VM: it
// asks the guest to flush + freeze, copies the now-quiescent disk into the
// store, then asks it to thaw via a deferred RESUME so a snapshot failure (or a
// panic) never leaves the guest's disk frozen.
func (r *Runtime) snapshotRunning(ctx context.Context, m *machine) error {
	if r.store == nil || m.worldDisk == "" || m.vsockUDS == "" {
		return fmt.Errorf("firecracker: live snapshot not available for vm %s", m.id)
	}

	conn, err := dialVsockControl(m.vsockUDS, runspec.VsockControlPort)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(snapshotDeadline))

	if err := snapCommand(conn, runspec.SnapPrepare); err != nil {
		return fmt.Errorf("firecracker: prepare snapshot: %w", err)
	}
	// The disk is frozen now; guarantee a thaw on every path out.
	defer func() { _ = snapCommand(conn, runspec.SnapResume) }()

	if err := snapshotWorldDisk(ctx, r.store, m.worldKey, m.worldDisk); err != nil {
		return fmt.Errorf("firecracker: snapshot frozen disk: %w", err)
	}
	return nil
}

// dialVsockControl opens the guest control connection through the VM's vsock
// UDS, completing Firecracker's CONNECT handshake.
func dialVsockControl(uds string, port int) (net.Conn, error) {
	conn, err := net.DialTimeout("unix", uds, vsockDialTimeout)
	if err != nil {
		return nil, fmt.Errorf("firecracker: dial vsock uds %s: %w", uds, err)
	}
	_ = conn.SetDeadline(time.Now().Add(vsockDialTimeout))
	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", port); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("firecracker: vsock connect: %w", err)
	}
	line, err := readLine(conn)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("firecracker: vsock connect reply: %w", err)
	}
	if !strings.HasPrefix(line, "OK") {
		_ = conn.Close()
		return nil, fmt.Errorf("firecracker: vsock connect refused: %q", line)
	}
	return conn, nil
}

// snapCommand sends one control command and checks the guest's reply.
func snapCommand(conn net.Conn, cmd string) error {
	if _, err := fmt.Fprintf(conn, "%s\n", cmd); err != nil {
		return fmt.Errorf("send %s: %w", cmd, err)
	}
	line, err := readLine(conn)
	if err != nil {
		return fmt.Errorf("read %s reply: %w", cmd, err)
	}
	if line != runspec.SnapOK {
		return fmt.Errorf("%s rejected: %q", cmd, line)
	}
	return nil
}

// readLine reads a single newline-terminated line byte by byte, so it never
// consumes bytes past the line (the protocol is strict request/response, and we
// keep using the raw conn for the next command).
func readLine(conn net.Conn) (string, error) {
	var b strings.Builder
	buf := make([]byte, 1)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			if buf[0] == '\n' {
				return strings.TrimRight(b.String(), "\r"), nil
			}
			b.WriteByte(buf[0])
		}
		if err != nil {
			return "", err
		}
	}
}
