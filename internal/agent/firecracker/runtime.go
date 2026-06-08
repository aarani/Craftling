package firecracker

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/aarani/craftling-go/internal/agent"
	"github.com/aarani/craftling-go/internal/runspec"
	"github.com/google/uuid"
)

// Runtime is the agent.Runtime backed by real Firecracker microVMs. It owns the
// VMs running on this host: their processes, working directories, and writable
// rootfs images. It is safe for concurrent use by the agent HTTP server.
type Runtime struct {
	cfg Config

	// dp and ipam are non-nil only when the NAT dataplane is enabled
	// (cfg.UplinkDevice set). dp is the shared eBPF collection loaded once;
	// ipam hands out per-VM addresses and host ports.
	dp   *natDataplane
	ipam *ipam

	mu  sync.Mutex
	vms map[string]*machine
}

// compile-time check that the driver satisfies the agent contract.
var _ agent.Runtime = (*Runtime)(nil)

// New constructs a Firecracker Runtime, validating host artifacts and ensuring
// the working directory exists.
func New(cfg Config) (*Runtime, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.WorkDir, 0o750); err != nil {
		return nil, fmt.Errorf("firecracker: work dir: %w", err)
	}

	r := &Runtime{cfg: cfg, vms: make(map[string]*machine)}

	// Bring up the shared eBPF NAT dataplane once, here, when an uplink is
	// configured. A failure is fatal: a half-networked host would boot VMs that
	// can't reach the internet, which is worse than refusing to start.
	if cfg.natEnabled() {
		dc, err := cfg.dataplaneConfig()
		if err != nil {
			return nil, err
		}
		ip, err := newIPAM(dc.subnet, dc.gatewayIP, dc.gatewayMAC, dc.portMin, dc.portMax)
		if err != nil {
			return nil, err
		}
		dp, err := newDataplane(dc)
		if err != nil {
			return nil, err
		}
		r.dp = dp
		r.ipam = ip
	}
	return r, nil
}

// Close tears down the NAT dataplane (detaching all eBPF programs). It is safe
// to call when the dataplane was never enabled.
func (r *Runtime) Close() {
	if r.dp != nil {
		r.dp.Close()
	}
}

// Provision creates a per-VM working dir + writable rootfs and boots a microVM.
func (r *Runtime) Provision(ctx context.Context, spec agent.VMSpec) (*agent.VM, error) {
	if spec.CPUs <= 0 || spec.MemoryMB <= 0 {
		return nil, fmt.Errorf("firecracker: invalid spec: cpus=%d memory_mb=%d", spec.CPUs, spec.MemoryMB)
	}
	baseImage, err := r.cfg.imageFor(spec.Version)
	if err != nil {
		return nil, err
	}

	id := "vm-" + uuid.NewString()
	dir := filepath.Join(r.cfg.WorkDir, id)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("firecracker: vm dir: %w", err)
	}

	// A per-VM writable copy of the base image. It survives stop/start so the
	// world persists across a restart on this host (cross-host persistence is P5).
	rootfs := filepath.Join(dir, "rootfs.ext4")
	if err := copyFile(baseImage, rootfs); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("firecracker: stage rootfs: %w", err)
	}

	runSpec := spec.RunSpec
	var vmnet vmNet
	// The NAT dataplane needs the in-VM init agent to apply per-VM addressing,
	// which only exists on the MMDS/runspec path. Legacy ext4 VMs (no runspec)
	// stay MMDS-only even when the dataplane is enabled for other VMs.
	if r.dp != nil && runSpec != nil {
		n, err := r.ipam.allocate()
		if err != nil {
			_ = os.RemoveAll(dir)
			return nil, err
		}
		vmnet = n
		// Copy the caller's runspec and attach the guest network config so we
		// never mutate a shared/caller-owned struct.
		rs := *runSpec
		rs.Net = &runspec.NetConfig{
			Interface:  runspec.MMDSInterface,
			Address:    vmnet.VMIP.String(),
			PrefixLen:  vmnet.PrefixLen,
			Gateway:    vmnet.GatewayIP.String(),
			GatewayMAC: vmnet.GatewayMAC.String(),
		}
		runSpec = &rs
	}

	m := &machine{
		id:          id,
		serverID:    spec.ServerID,
		dir:         dir,
		socket:      filepath.Join(dir, "firecracker.sock"),
		rootfs:      rootfs,
		kernel:      r.cfg.KernelPath,
		binary:      r.cfg.BinaryPath,
		bootArgs:    r.cfg.BootArgs,
		vcpus:       spec.CPUs,
		memoryMB:    spec.MemoryMB,
		runSpec:     runSpec,
		tapName:     tapNameFor(id),
		dp:          r.dp,
		net:         vmnet,
		servicePort: defaultMinecraftPort,
	}
	if err := m.boot(ctx); err != nil {
		r.releaseNet(vmnet)
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("firecracker: boot vm: %w", err)
	}

	r.mu.Lock()
	r.vms[id] = m
	r.mu.Unlock()
	return r.vmView(m), nil
}

// Start re-boots a stopped VM from its existing rootfs. It is a no-op for an
// already-running VM and ErrVMNotFound for an unknown id.
func (r *Runtime) Start(ctx context.Context, vmID string) (*agent.VM, error) {
	r.mu.Lock()
	m, ok := r.vms[vmID]
	r.mu.Unlock()
	if !ok {
		return nil, agent.ErrVMNotFound
	}
	if m.running() {
		return r.vmView(m), nil
	}
	if err := m.boot(ctx); err != nil {
		return nil, fmt.Errorf("firecracker: restart vm: %w", err)
	}
	return r.vmView(m), nil
}

// Stop halts a VM's process without destroying its rootfs (idempotent). An
// unknown VM is treated as already gone.
func (r *Runtime) Stop(ctx context.Context, vmID string) error {
	r.mu.Lock()
	m, ok := r.vms[vmID]
	r.mu.Unlock()
	if !ok {
		return nil
	}
	m.shutdown(ctx)
	return nil
}

// Deprovision force-stops a VM and removes its working directory (idempotent).
func (r *Runtime) Deprovision(_ context.Context, vmID string) error {
	r.mu.Lock()
	m, ok := r.vms[vmID]
	if ok {
		delete(r.vms, vmID)
	}
	r.mu.Unlock()
	if !ok {
		return nil
	}
	m.kill()
	if m.runSpec != nil {
		// Detach the dataplane and release the VM's address/port before the
		// TAP disappears. Best-effort: the MMDS TAP outlives the Firecracker
		// process, so destroy it here too. A failure only leaks a host device;
		// it must not block teardown of the VM's working directory.
		if r.dp != nil {
			r.dp.withdrawVM(m.tapName, m.net)
			r.releaseNet(m.net)
		}
		_ = deleteTAP(m.tapName)
	}
	if err := os.RemoveAll(m.dir); err != nil {
		return fmt.Errorf("firecracker: remove vm dir: %w", err)
	}
	return nil
}

// Status reports a VM's observed state: running if its process is alive, stopped
// if it is tracked but not running, missing for an unknown id.
func (r *Runtime) Status(_ context.Context, vmID string) (*agent.VM, error) {
	r.mu.Lock()
	m, ok := r.vms[vmID]
	r.mu.Unlock()
	if !ok {
		return &agent.VM{ID: vmID, State: agent.StateMissing}, nil
	}
	return r.vmView(m), nil
}

// releaseNet returns a VM's address/port to the IPAM pool. No-op when the
// dataplane is disabled or the vmNet is empty.
func (r *Runtime) releaseNet(n vmNet) {
	if r.ipam != nil {
		r.ipam.release(n)
	}
}

// vmView renders a machine as the agent.VM the API returns, deriving state from
// process liveness. With the NAT dataplane the connect endpoint is the host's
// advertise address and the VM's IPAM-allocated public host port; otherwise it
// falls back to the standard in-VM port.
func (r *Runtime) vmView(m *machine) *agent.VM {
	state := agent.StateStopped
	if m.running() {
		state = agent.StateRunning
	}
	port := defaultMinecraftPort
	if r.dp != nil && m.net.HostPort != 0 {
		port = int(m.net.HostPort)
	}
	return &agent.VM{
		ID:       m.id,
		ServerID: m.serverID,
		Host:     r.cfg.AdvertiseHost,
		Port:     port,
		State:    state,
	}
}

// copyFile copies src to dst, creating dst (truncating if present).
func copyFile(src, dst string) error {
	in, err := os.Open(src) //nolint:gosec // driver-controlled path
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o640) //nolint:gosec // driver-controlled path
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
