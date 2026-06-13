package firecracker

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/aarani/craftling-go/internal/agent"
	"github.com/aarani/craftling-go/internal/runspec"
	"github.com/aarani/craftling-go/internal/storage"
	"github.com/google/uuid"
	"go.uber.org/zap"
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

	// store is the durable world store (P5b), non-nil when WorldPersistence is
	// on and a store is configured. Provision restores from it, Stop snapshots
	// into it, Deprovision deletes from it.
	store storage.WorldStore

	// done is closed by Close to stop the periodic snapshot sweep (P5c); sweepWG
	// tracks the sweeper goroutine so Close can wait for it to exit.
	done    chan struct{}
	sweepWG sync.WaitGroup

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

	r := &Runtime{cfg: cfg, vms: make(map[string]*machine), done: make(chan struct{})}
	if cfg.persistEnabled() {
		r.store = cfg.WorldStore
	}
	if r.store != nil && cfg.SnapshotInterval > 0 {
		r.sweepWG.Add(1)
		go r.snapshotSweep(cfg.SnapshotInterval)
	}

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

// Close stops the periodic snapshot sweep and tears down the NAT dataplane
// (detaching all eBPF programs). It is safe to call when neither was enabled.
func (r *Runtime) Close() {
	close(r.done)
	r.sweepWG.Wait()
	if r.dp != nil {
		r.dp.Close()
	}
}

// snapshotSweep periodically takes an application-consistent snapshot of every
// running VM, bounding crash data-loss to one interval. It runs each VM's
// snapshot serially (each freezes that VM's disk only briefly); failures are
// logged, never fatal — a missed interval is recoverable, a crashed sweeper is
// not.
func (r *Runtime) snapshotSweep(interval time.Duration) {
	defer r.sweepWG.Done()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-r.done:
			return
		case <-ticker.C:
			for _, m := range r.snapshotCandidates() {
				ctx, cancel := context.WithTimeout(context.Background(), snapshotDeadline)
				if err := r.snapshotRunning(ctx, m); err != nil {
					r.cfg.Logger.Warn("periodic world snapshot failed",
						zap.String("vm", m.id), zap.String("server", m.serverID), zap.Error(err))
				} else {
					r.cfg.Logger.Debug("periodic world snapshot taken",
						zap.String("vm", m.id), zap.String("server", m.serverID))
				}
				cancel()
			}
		}
	}
}

// snapshotCandidates returns a snapshot of the running, live-snapshot-capable
// machines, taken under the lock so the sweep can then snapshot each without
// holding it (a freeze + disk read is slow and must not block the VM API).
func (r *Runtime) snapshotCandidates() []*machine {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*machine
	for _, m := range r.vms {
		if m.vsockUDS != "" && m.worldDisk != "" && m.running() {
			out = append(out, m)
		}
	}
	return out
}

// Provision creates a per-VM working dir + writable rootfs and boots a microVM.
func (r *Runtime) Provision(ctx context.Context, spec agent.VMSpec) (*agent.VM, error) {
	if spec.CPUs <= 0 || spec.MemoryMB <= 0 {
		return nil, fmt.Errorf("firecracker: invalid spec: cpus=%d memory_mb=%d", spec.CPUs, spec.MemoryMB)
	}
	id := "vm-" + uuid.NewString()
	dir := filepath.Join(r.cfg.WorkDir, id)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("firecracker: vm dir: %w", err)
	}

	// Resolve the root drive. Two paths:
	//   - OCI image (spec.Image set): build (or reuse) a shared read-only
	//     squashfs rootfs from the image, taking its run spec as the MMDS base.
	//     The squashfs lives in the content-addressed image cache and is shared
	//     across VMs of the same digest, so it is attached in place — not copied
	//     per VM — and the per-VM dir holds only sockets/logs/world disk.
	//   - Legacy (no image): a per-VM writable copy of the per-version ext4
	//     base, mounted read-write, with whatever run spec the caller supplied.
	var (
		rootfs       string
		rootReadOnly bool
		bootArgs     = r.cfg.BootArgs
		runSpec      = spec.RunSpec
	)
	if spec.Image != "" {
		if r.cfg.ImageStore == nil {
			_ = os.RemoveAll(dir)
			return nil, fmt.Errorf("firecracker: spec names image %q but no image store is configured", spec.Image)
		}
		path, ociSpec, err := r.cfg.ImageStore.Ensure(ctx, spec.Image, spec.ImageDigest)
		if err != nil {
			_ = os.RemoveAll(dir)
			return nil, fmt.Errorf("firecracker: prepare rootfs for %q: %w", spec.Image, err)
		}
		rootfs = path
		rootReadOnly = true
		bootArgs = ociBootArgs
		rs := ociSpec
		runSpec = &rs
	} else {
		baseImage, err := r.cfg.imageFor(spec.Version)
		if err != nil {
			_ = os.RemoveAll(dir)
			return nil, err
		}
		// A per-VM writable copy of the base image. It survives stop/start so the
		// world persists across a restart on this host (cross-host persistence is P5).
		rootfs = filepath.Join(dir, "rootfs.ext4")
		if err := copyFile(baseImage, rootfs); err != nil {
			_ = os.RemoveAll(dir)
			return nil, fmt.Errorf("firecracker: stage rootfs: %w", err)
		}
	}

	// Both the NAT dataplane and world persistence augment the runspec the
	// guest init fetches, and both only apply on the MMDS/runspec path —
	// legacy ext4 VMs (no runspec) have their own init and stay MMDS-only.
	// We already copied the spec above (OCI path) or took the caller's; copy
	// once more before mutating so we never write through a shared pointer.
	var vmnet vmNet
	var worldDisk, worldKey string
	if runSpec != nil && (r.dp != nil || r.cfg.persistEnabled()) {
		rs := *runSpec

		if r.dp != nil {
			n, err := r.ipam.allocate()
			if err != nil {
				_ = os.RemoveAll(dir)
				return nil, err
			}
			vmnet = n
			rs.Net = &runspec.NetConfig{
				Interface:  runspec.MMDSInterface,
				Address:    vmnet.VMIP.String(),
				PrefixLen:  vmnet.PrefixLen,
				Gateway:    vmnet.GatewayIP.String(),
				GatewayMAC: vmnet.GatewayMAC.String(),
			}
		}

		if r.cfg.persistEnabled() {
			target, ok := persistTarget(rs.WorkingDir)
			if !ok {
				r.releaseNet(vmnet)
				_ = os.RemoveAll(dir)
				return nil, fmt.Errorf("firecracker: world persistence requires an absolute, non-root WorkingDir, got %q", rs.WorkingDir)
			}
			// Key the disk by server id so it can outlive this VM instance and
			// a host reschedule (the world store is keyed the same way); fall
			// back to the VM id when a spec carries no server id.
			worldKey = spec.ServerID
			if worldKey == "" {
				worldKey = id
			}
			wd := r.cfg.worldDiskPath(worldKey)
			if err := r.prepareWorldDisk(ctx, worldKey, wd); err != nil {
				r.releaseNet(vmnet)
				_ = os.RemoveAll(dir)
				return nil, fmt.Errorf("firecracker: world disk: %w", err)
			}
			worldDisk = wd
			rs.Persist = &runspec.PersistConfig{Device: worldDevice, Mountpoint: target}

			// When a store is configured, enable live snapshots: the guest
			// gets a Quiesce block (flush + freeze) and we attach a vsock
			// device below so the host can drive it.
			if r.cfg.liveSnapshotEnabled() {
				q := &runspec.QuiesceConfig{}
				if r.cfg.RCONPassword != "" {
					q.RCONAddress = fmt.Sprintf("127.0.0.1:%d", r.cfg.RCONPort)
					q.RCONPassword = r.cfg.RCONPassword
				}
				rs.Quiesce = q
			}
		}

		runSpec = &rs
	}

	// The host-side vsock UDS lives in the per-VM dir; set it when this VM has
	// a Quiesce block so configure() attaches the device.
	var vsockUDS string
	if runSpec != nil && runSpec.Quiesce != nil {
		vsockUDS = filepath.Join(dir, "vsock.sock")
	}

	m := &machine{
		id:           id,
		serverID:     spec.ServerID,
		dir:          dir,
		socket:       filepath.Join(dir, "firecracker.sock"),
		rootfs:       rootfs,
		rootReadOnly: rootReadOnly,
		kernel:       r.cfg.KernelPath,
		binary:       r.cfg.BinaryPath,
		bootArgs:     bootArgs,
		vcpus:        spec.CPUs,
		memoryMB:     spec.MemoryMB,
		runSpec:      runSpec,
		tapName:      tapNameFor(id),
		worldDisk:    worldDisk,
		worldKey:     worldKey,
		vsockUDS:     vsockUDS,
		dp:           r.dp,
		net:          vmnet,
		servicePort:  defaultMinecraftPort,
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
	// The guest has powered off (synced) by the time shutdown returns, so the
	// disk image is consistent — snapshot it to the durable store now, so a
	// later delete or reschedule can restore the world. A snapshot failure is
	// returned, not swallowed: the stop succeeded but the world wasn't saved,
	// and the reconciler should retry rather than silently risk data loss.
	if r.store != nil && m.worldDisk != "" {
		if err := snapshotWorldDisk(ctx, r.store, m.worldKey, m.worldDisk); err != nil {
			return fmt.Errorf("firecracker: snapshot world on stop: %w", err)
		}
	}
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
	// Destroy the world disk along with the VM. This is the point P5b will
	// snapshot-then-upload before removing, so a deprovision becomes a safe
	// teardown rather than data loss; for now destroy means destroy. Removing
	// the keyed parent dir (DataDir/<key>) takes the disk with it.
	if m.worldDisk != "" {
		if err := os.RemoveAll(filepath.Dir(m.worldDisk)); err != nil {
			return fmt.Errorf("firecracker: remove world disk: %w", err)
		}
		// Delete the durable copy too: deprovision is a server delete, so the
		// world is meant to be gone. Best-effort — an orphaned blob is harmless
		// (a later GC can sweep it) and must not block teardown.
		if r.store != nil {
			_ = r.store.Delete(context.Background(), m.worldKey)
		}
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

// prepareWorldDisk readies a server's world disk at diskPath before boot. When
// a world store holds a snapshot for this server it restores that (the
// reschedule / re-create path); otherwise it formats a fresh empty disk. A
// restored image already carries an ext4, so no mkfs is needed.
func (r *Runtime) prepareWorldDisk(ctx context.Context, serverID, diskPath string) error {
	if r.store != nil {
		ok, err := r.store.Exists(ctx, serverID)
		if err != nil {
			return fmt.Errorf("check world store: %w", err)
		}
		if ok {
			return restoreWorldDisk(ctx, r.store, serverID, diskPath)
		}
	}
	return ensureWorldDisk(diskPath, r.cfg.WorldDiskMB, r.cfg.MkfsExt4Path)
}

// Snapshot takes an on-demand application-consistent snapshot of a running VM's
// world (P5c). It is the same freeze→store→thaw exchange the periodic sweep
// uses. ErrVMNotFound for an unknown id; an error when live snapshots aren't
// configured for this VM (no store / no vsock).
func (r *Runtime) Snapshot(ctx context.Context, vmID string) error {
	r.mu.Lock()
	m, ok := r.vms[vmID]
	r.mu.Unlock()
	if !ok {
		return agent.ErrVMNotFound
	}
	if r.store == nil || m.vsockUDS == "" || m.worldDisk == "" {
		return fmt.Errorf("firecracker: live snapshot not available for vm %s", vmID)
	}
	return r.snapshotRunning(ctx, m)
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
