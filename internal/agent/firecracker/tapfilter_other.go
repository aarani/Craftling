//go:build !linux

package firecracker

import "net"

// TapFlowEvent and OnTapFlowEvent are declared on all platforms so callers can
// reference them; the filter itself is a no-op off Linux.
type TapFlowEvent struct {
	Tap     string
	SrcIP   net.IP
	DstIP   net.IP
	SrcPort uint16
	DstPort uint16
	Length  uint16
	Proto   uint8
	Ingress bool
	Dropped bool
}

var OnTapFlowEvent func(TapFlowEvent)

func maybeAttachTAPFilter(string) error { return nil }

func attachTAPFilter(string, uint16, bool) error { return errTAPUnsupported }

func detachTAPFilter(string) {}

func tapFilterStats(string) (ingress, egress, dropped uint64, ok bool) {
	return 0, 0, 0, false
}
