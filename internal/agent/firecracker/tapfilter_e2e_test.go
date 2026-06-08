//go:build bpf && linux

package firecracker

import (
	"net"
	"testing"
	"time"

	"github.com/aarani/craftling-go/internal/agent/firecracker/bpf"
	"github.com/cilium/ebpf/ringbuf"
	"github.com/cilium/ebpf/rlimit"
	"golang.org/x/sys/unix"
)

// loadTapfilter loads the compiled tapfilter objects into the kernel, writes the
// watch config, and returns them with a cleanup. A load failure here is a real
// failure: requireBPFRoot has already established the kernel is new enough.
func loadTapfilter(t *testing.T, port uint16, drop bool) *bpf.TapfilterObjects {
	t.Helper()
	_ = rlimit.RemoveMemlock()
	objs := &bpf.TapfilterObjects{}
	if err := bpf.LoadTapfilterObjects(objs, nil); err != nil {
		t.Fatalf("load tapfilter objects (kernel >= 6.6?): %v", err)
	}
	t.Cleanup(func() { _ = objs.Close() })

	cfg := bpf.TapfilterConfig{Port: port}
	if drop {
		cfg.Drop = 1
	}
	if err := objs.ConfigMap.Put(uint32(0), &cfg); err != nil {
		t.Fatalf("write tapfilter config: %v", err)
	}
	return objs
}

func tapStat(t *testing.T, objs *bpf.TapfilterObjects, idx uint32) uint64 {
	t.Helper()
	var v uint64
	if err := objs.Stats.Lookup(idx, &v); err != nil {
		t.Fatalf("stats[%d]: %v", idx, err)
	}
	return v
}

// TestTAPFilterProgramLogic drives the tapfilter programs directly with
// BPF_PROG_TEST_RUN — no devices, fully deterministic — and asserts the pass /
// drop verdict, the per-direction match counters, and the ringbuf event for a
// matching packet. This is the rigorous check of the program's packet logic.
func TestTAPFilterProgramLogic(t *testing.T) {
	requireBPFRoot(t)

	const watch = 25565
	sip := net.IPv4(10, 1, 0, 2)
	dip := net.IPv4(10, 1, 0, 1)

	t.Run("observe pass and count", func(t *testing.T) {
		objs := loadTapfilter(t, watch, false)

		// A TCP SYN to the watched port, presented as guest TX (ingress).
		match := ethIPv4(macA, macB, sip, dip, unix.IPPROTO_TCP, tcpSeg(40000, watch))
		ret, _, err := objs.TapIngress.Test(match)
		if err != nil {
			t.Fatalf("run tap_ingress: %v", err)
		}
		if ret != tcActOK {
			t.Fatalf("observe-only verdict = %d, want TC_ACT_OK(%d)", ret, tcActOK)
		}
		if got := tapStat(t, objs, 0); got != 1 {
			t.Fatalf("ingress matches = %d, want 1", got)
		}

		// A UDP datagram to the watched port the other way (host->guest, egress).
		egr := ethIPv4(macB, macA, dip, sip, unix.IPPROTO_UDP, udpSeg(watch, 40000, []byte("hi")))
		if ret, _, err := objs.TapEgress.Test(egr); err != nil || ret != tcActOK {
			t.Fatalf("run tap_egress = (%d, %v), want (TC_ACT_OK, nil)", ret, err)
		}
		if got := tapStat(t, objs, 1); got != 1 {
			t.Fatalf("egress matches = %d, want 1", got)
		}
		// Nothing was dropped in observe-only mode.
		if got := tapStat(t, objs, 2); got != 0 {
			t.Fatalf("dropped = %d, want 0", got)
		}
	})

	t.Run("non-matching port passes untouched", func(t *testing.T) {
		objs := loadTapfilter(t, watch, false)
		other := ethIPv4(macA, macB, sip, dip, unix.IPPROTO_TCP, tcpSeg(40000, 80))
		if ret, _, err := objs.TapIngress.Test(other); err != nil || ret != tcActOK {
			t.Fatalf("run tap_ingress = (%d, %v), want (TC_ACT_OK, nil)", ret, err)
		}
		if got := tapStat(t, objs, 0); got != 0 {
			t.Fatalf("unwatched port bumped ingress matches to %d, want 0", got)
		}
	})

	t.Run("drop mode shoots and counts the drop", func(t *testing.T) {
		objs := loadTapfilter(t, watch, true)
		match := ethIPv4(macA, macB, sip, dip, unix.IPPROTO_TCP, tcpSeg(40000, watch))
		ret, _, err := objs.TapIngress.Test(match)
		if err != nil {
			t.Fatalf("run tap_ingress: %v", err)
		}
		if ret != tcActShot {
			t.Fatalf("drop-mode verdict = %d, want TC_ACT_SHOT(%d)", ret, tcActShot)
		}
		if got := tapStat(t, objs, 2); got != 1 {
			t.Fatalf("dropped = %d, want 1", got)
		}
	})

	t.Run("emits a decodable ringbuf event", func(t *testing.T) {
		objs := loadTapfilter(t, watch, false)
		rd, err := ringbuf.NewReader(objs.Events)
		if err != nil {
			t.Fatalf("open events ringbuf: %v", err)
		}
		defer rd.Close()

		match := ethIPv4(macA, macB, sip, dip, unix.IPPROTO_UDP, udpSeg(40000, watch, []byte("payload")))
		if _, _, err := objs.TapIngress.Test(match); err != nil {
			t.Fatalf("run tap_ingress: %v", err)
		}

		rd.SetDeadline(time.Now().Add(2 * time.Second))
		rec, err := rd.Read()
		if err != nil {
			t.Fatalf("read event: %v", err)
		}
		ev, ok := decodeFlowEvent("test", rec.RawSample)
		if !ok {
			t.Fatalf("decodeFlowEvent failed on %d bytes", len(rec.RawSample))
		}
		if !ev.Ingress {
			t.Fatalf("event ingress = false, want true (guest TX)")
		}
		if ev.DstPort != watch {
			t.Fatalf("event dport = %d, want %d", ev.DstPort, watch)
		}
		if ev.Proto != unix.IPPROTO_UDP {
			t.Fatalf("event proto = %d, want UDP(%d)", ev.Proto, unix.IPPROTO_UDP)
		}
		if !ev.SrcIP.Equal(sip) || !ev.DstIP.Equal(dip) {
			t.Fatalf("event addrs = %s->%s, want %s->%s", ev.SrcIP, ev.DstIP, sip, dip)
		}
	})
}

// TestTAPFilterAttachLiveTraffic exercises the *production* wiring end to end:
// attachTAPFilter loads the program, attaches it to both TCX directions of a
// real veth, and drains the ringbuf to OnTapFlowEvent. We then send a real UDP
// datagram from the host out the veth (so it hits the egress hook) and assert
// the callback fires with the decoded flow and that the egress counter moved.
func TestTAPFilterAttachLiveTraffic(t *testing.T) {
	requireBPFRoot(t)

	const watch = 39999
	v := setupVethNS(t, "tf", "10.250.1")

	events := make(chan TapFlowEvent, 16)
	prev := OnTapFlowEvent
	OnTapFlowEvent = func(ev TapFlowEvent) {
		// Non-blocking: never wedge the drain goroutine if the test has moved on.
		select {
		case events <- ev:
		default:
		}
	}
	t.Cleanup(func() { OnTapFlowEvent = prev })

	if err := attachTAPFilter(v.Host, watch, false); err != nil {
		t.Fatalf("attachTAPFilter(%s): %v", v.Host, err)
	}
	t.Cleanup(func() { detachTAPFilter(v.Host) })

	// Idempotent per device: a second attach is a no-op, not an error.
	if err := attachTAPFilter(v.Host, watch, false); err != nil {
		t.Fatalf("second attachTAPFilter should be a no-op, got: %v", err)
	}

	// Send UDP toward the peer's address:watch. The datagram is transmitted out
	// the host veth (egress hook) regardless of whether anything is listening.
	// First packets can be consumed by ARP resolution, so send until we see the
	// event or time out.
	conn, err := net.Dial("udp", net.JoinHostPort(v.PeerIP.String(), "39999"))
	if err != nil {
		t.Fatalf("dial udp: %v", err)
	}
	defer conn.Close()

	deadline := time.After(5 * time.Second)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		if _, err := conn.Write([]byte("ping")); err != nil {
			t.Fatalf("send udp: %v", err)
		}
		select {
		case ev := <-events:
			if ev.Tap != v.Host {
				t.Fatalf("event tap = %q, want %q", ev.Tap, v.Host)
			}
			if ev.DstPort != watch {
				t.Fatalf("event dport = %d, want %d", ev.DstPort, watch)
			}
			if ev.Ingress {
				t.Fatalf("host-transmitted packet reported as ingress; want egress")
			}
			if !ev.DstIP.Equal(v.PeerIP.To4()) {
				t.Fatalf("event dst = %s, want %s", ev.DstIP, v.PeerIP)
			}
			// The production stats accessor must agree the egress hook fired.
			if _, egress, _, ok := tapFilterStats(v.Host); !ok || egress == 0 {
				t.Fatalf("tapFilterStats egress = %d (ok=%v), want >= 1", egress, ok)
			}
			return
		case <-tick.C:
			continue
		case <-deadline:
			ingress, egress, dropped, ok := tapFilterStats(v.Host)
			t.Fatalf("no flow event within timeout (stats ingress=%d egress=%d dropped=%d ok=%v)",
				ingress, egress, dropped, ok)
		}
	}
}

// TestTAPFilterMaybeAttachGate verifies the env-gated entry point: no port set
// means no filter; an explicit port attaches one. This is the hook tap_linux.go
// calls from createTAP, so it guards the opt-in contract.
func TestTAPFilterMaybeAttachGate(t *testing.T) {
	requireBPFRoot(t)
	v := setupVethNS(t, "tg", "10.250.2")

	t.Setenv(envTAPFilterPort, "")
	if err := maybeAttachTAPFilter(v.Host); err != nil {
		t.Fatalf("maybeAttachTAPFilter with no port should be a no-op: %v", err)
	}
	if _, _, _, ok := tapFilterStats(v.Host); ok {
		t.Fatalf("filter attached despite unset %s", envTAPFilterPort)
	}

	t.Setenv(envTAPFilterPort, "25565")
	if err := maybeAttachTAPFilter(v.Host); err != nil {
		t.Fatalf("maybeAttachTAPFilter with port set: %v", err)
	}
	t.Cleanup(func() { detachTAPFilter(v.Host) })
	if _, _, _, ok := tapFilterStats(v.Host); !ok {
		t.Fatalf("filter not attached despite %s=25565", envTAPFilterPort)
	}
}
