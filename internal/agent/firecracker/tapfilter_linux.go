//go:build linux

package firecracker

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"sync"

	"github.com/aarani/craftling-go/internal/agent/firecracker/bpf"
	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
	"github.com/cilium/ebpf/rlimit"
)

// Environment knobs for the opt-in TAP filter. Kept as env vars so enabling the
// feature needs no changes to the runspec/machine plumbing; wire these to a
// RunSpec field when per-VM policy is required.
const (
	envTAPFilterPort = "CRAFTLING_TAP_FILTER_PORT" // TCP/UDP port to watch; unset/0 disables
	envTAPFilterDrop = "CRAFTLING_TAP_FILTER_DROP" // "1"/"true" => drop matching packets
)

// maybeAttachTAPFilter attaches the filter to name iff a watch port is
// configured via the environment. It is a no-op (nil) otherwise.
func maybeAttachTAPFilter(name string) error {
	raw := os.Getenv(envTAPFilterPort)
	if raw == "" {
		return nil
	}
	port, err := strconv.ParseUint(raw, 10, 16)
	if err != nil || port == 0 {
		return fmt.Errorf("%s=%q: must be a port in 1..65535", envTAPFilterPort, raw)
	}
	drop, _ := strconv.ParseBool(os.Getenv(envTAPFilterDrop))
	return attachTAPFilter(name, uint16(port), drop)
}

// TapFlowEvent is one packet observed on a TAP device's watched port. SrcIP and
// DstIP are 4-byte IPv4 addresses (network order, ready for net.IP); ports are
// host byte order.
type TapFlowEvent struct {
	Tap     string
	SrcIP   net.IP
	DstIP   net.IP
	SrcPort uint16
	DstPort uint16
	Length  uint16
	Proto   uint8 // unix.IPPROTO_TCP / IPPROTO_UDP
	Ingress bool  // true = guest transmitted this (host-side TAP ingress)
	Dropped bool
}

// OnTapFlowEvent, if set, is called for every flow event drained from a TAP
// filter's ringbuf. It runs on a per-filter goroutine, so it must be cheap and
// safe for concurrent calls across multiple VMs. When nil, events are drained
// and discarded (counters in the stats map are still maintained in the kernel).
var OnTapFlowEvent func(TapFlowEvent)

// tapFilter is the loaded eBPF objects and TCX links for one TAP device.
type tapFilter struct {
	tap    string
	objs   bpf.TapfilterObjects
	ingres link.Link
	egres  link.Link
	reader *ringbuf.Reader
	done   chan struct{}
}

var (
	tapFiltersMu sync.Mutex
	tapFilters   = map[string]*tapFilter{}
)

// attachTAPFilter loads the tapfilter program, attaches it to both directions of
// the named TAP via TCX, and starts draining flow events. port is the single
// TCP/UDP port to watch (host byte order); drop selects observe-only (false) or
// observe-and-drop (true). It is idempotent per TAP name. Requires CAP_BPF and
// CAP_NET_ADMIN and a kernel >= 6.6 (TCX).
func attachTAPFilter(tap string, port uint16, drop bool) error {
	tapFiltersMu.Lock()
	defer tapFiltersMu.Unlock()
	if _, ok := tapFilters[tap]; ok {
		return nil
	}

	iface, err := net.InterfaceByName(tap)
	if err != nil {
		return fmt.Errorf("lookup tap %q: %w", tap, err)
	}

	// Best effort: harmless on >=5.11 where memcg accounting replaces rlimit.
	_ = rlimit.RemoveMemlock()

	f := &tapFilter{tap: tap, done: make(chan struct{})}
	if err := bpf.LoadTapfilterObjects(&f.objs, nil); err != nil {
		return fmt.Errorf("load tapfilter objects: %w", err)
	}

	cfg := bpf.TapfilterConfig{Port: port}
	if drop {
		cfg.Drop = 1
	}
	if err := f.objs.ConfigMap.Put(uint32(0), &cfg); err != nil {
		f.closeObjs()
		return fmt.Errorf("write config: %w", err)
	}

	f.ingres, err = link.AttachTCX(link.TCXOptions{
		Interface: iface.Index,
		Program:   f.objs.TapIngress,
		Attach:    ebpf.AttachTCXIngress,
	})
	if err != nil {
		f.closeObjs()
		return fmt.Errorf("attach tcx ingress: %w", err)
	}
	f.egres, err = link.AttachTCX(link.TCXOptions{
		Interface: iface.Index,
		Program:   f.objs.TapEgress,
		Attach:    ebpf.AttachTCXEgress,
	})
	if err != nil {
		_ = f.ingres.Close()
		f.closeObjs()
		return fmt.Errorf("attach tcx egress: %w", err)
	}

	f.reader, err = ringbuf.NewReader(f.objs.Events)
	if err != nil {
		_ = f.egres.Close()
		_ = f.ingres.Close()
		f.closeObjs()
		return fmt.Errorf("open ringbuf: %w", err)
	}

	go f.drain()
	tapFilters[tap] = f
	return nil
}

// detachTAPFilter tears down the filter for a TAP, if any. Safe to call for a
// TAP that was never filtered.
func detachTAPFilter(tap string) {
	tapFiltersMu.Lock()
	f, ok := tapFilters[tap]
	if ok {
		delete(tapFilters, tap)
	}
	tapFiltersMu.Unlock()
	if !ok {
		return
	}
	// Closing the reader unblocks drain(); links and objects follow.
	_ = f.reader.Close()
	<-f.done
	_ = f.egres.Close()
	_ = f.ingres.Close()
	f.closeObjs()
}

// tapFilterStats returns (ingressMatches, egressMatches, dropped) for a TAP, or
// false if no filter is attached.
func tapFilterStats(tap string) (ingress, egress, dropped uint64, ok bool) {
	tapFiltersMu.Lock()
	f, present := tapFilters[tap]
	tapFiltersMu.Unlock()
	if !present {
		return 0, 0, 0, false
	}
	ingress = f.statAt(0)
	egress = f.statAt(1)
	dropped = f.statAt(2)
	return ingress, egress, dropped, true
}

func (f *tapFilter) statAt(idx uint32) uint64 {
	var v uint64
	_ = f.objs.Stats.Lookup(idx, &v)
	return v
}

func (f *tapFilter) closeObjs() { _ = f.objs.Close() }

func (f *tapFilter) drain() {
	defer close(f.done)
	for {
		rec, err := f.reader.Read()
		if err != nil {
			if errors.Is(err, ringbuf.ErrClosed) {
				return
			}
			continue
		}
		if OnTapFlowEvent == nil {
			continue
		}
		ev, ok := decodeFlowEvent(f.tap, rec.RawSample)
		if ok {
			OnTapFlowEvent(ev)
		}
	}
}

// decodeFlowEvent parses the raw ringbuf sample into a TapFlowEvent. The wire
// layout matches struct event in tapfilter.c (little-endian host).
func decodeFlowEvent(tap string, raw []byte) (TapFlowEvent, bool) {
	if len(raw) < 18 {
		return TapFlowEvent{}, false
	}
	le := binary.LittleEndian
	return TapFlowEvent{
		Tap:     tap,
		SrcIP:   net.IP(append([]byte(nil), raw[0:4]...)),
		DstIP:   net.IP(append([]byte(nil), raw[4:8]...)),
		SrcPort: le.Uint16(raw[8:10]),
		DstPort: le.Uint16(raw[10:12]),
		Length:  le.Uint16(raw[12:14]),
		Ingress: raw[14] != 0,
		Dropped: raw[15] != 0,
		Proto:   raw[16],
	}, true
}
