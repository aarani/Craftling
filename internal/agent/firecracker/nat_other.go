//go:build !linux

package firecracker

import "net"

// NatFlowEvent and OnNatFlowEvent are declared on all platforms so callers can
// reference them; the dataplane itself is a no-op off Linux (the Firecracker
// host is always Linux/KVM — these stubs keep `go build ./...` green elsewhere).
type NatFlowEvent struct {
	VMIP    net.IP
	OrigSrc net.IP
	OrigDst net.IP
	NewSrc  net.IP
	NewDst  net.IP
	OrigSP  uint16
	OrigDP  uint16
	NewSP   uint16
	NewDP   uint16
	Length  uint16
	Proto   uint8
	Dir     uint8
	Verdict uint8
}

const (
	NatDirEgress      uint8 = 0
	NatDirEgressReply uint8 = 1
	NatDirInbound     uint8 = 2
	NatDirInboundRep  uint8 = 3
	NatDirDeny        uint8 = 4
)

var OnNatFlowEvent func(NatFlowEvent)

// natStats mirrors the Linux counter struct so platform-neutral callers compile.
type natStats struct {
	RxPkts, RxBytes, TxPkts, TxBytes, Drops, Conns uint64
}

// natDataplane is an empty placeholder off Linux; its methods are no-ops.
type natDataplane struct{}

func newDataplane(dataplaneConfig) (*natDataplane, error) { return nil, errTAPUnsupported }

func (*natDataplane) publishVM(string, vmNet, uint16) error { return errTAPUnsupported }

func (*natDataplane) withdrawVM(string, vmNet) {}

func (*natDataplane) vmStats(net.IP) (natStats, bool) { return natStats{}, false }

func (*natDataplane) Close() {}
