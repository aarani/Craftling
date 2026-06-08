//go:build kvm

// This integration test boots a real Firecracker microVM and therefore needs
// /dev/kvm plus host artifacts. It is gated behind the `kvm` build tag and kept
// out of the default CI lane (run on a self-hosted KVM runner — see P10):
//
//	FC_KERNEL=/path/vmlinux FC_IMAGE_DIR=/path/images FC_DEFAULT_IMAGE=base.ext4 \
//	  go test -tags kvm ./internal/agent/firecracker -run TestKVM -v
package firecracker

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/aarani/craftling-go/internal/agent"
)

// TestKVMLifecycle drives a real microVM through the full Runtime contract over
// the agent seam: provision (boots), stop (process gone, VM kept), start
// (re-boots from the same rootfs), and deprovision (gone).
func TestKVMLifecycle(t *testing.T) {
	kernel := os.Getenv("FC_KERNEL")
	imageDir := os.Getenv("FC_IMAGE_DIR")
	if kernel == "" || imageDir == "" {
		t.Skip("set FC_KERNEL and FC_IMAGE_DIR to run the KVM integration test")
	}

	rt, err := New(Config{
		BinaryPath:    os.Getenv("FC_BINARY"),
		KernelPath:    kernel,
		ImageDir:      imageDir,
		DefaultImage:  os.Getenv("FC_DEFAULT_IMAGE"),
		WorkDir:       t.TempDir(),
		AdvertiseHost: "127.0.0.1",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	spec := agent.VMSpec{
		ServerID: "kvm-it",
		Game:     "minecraft",
		Version:  os.Getenv("FC_TEST_VERSION"),
		CPUs:     1,
		MemoryMB: 256,
	}
	vm, err := rt.Provision(ctx, spec)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	// Always clean up the VM even if a later assertion fails.
	defer func() { _ = rt.Deprovision(context.Background(), vm.ID) }()

	if vm.ID == "" || vm.State != agent.StateRunning {
		t.Fatalf("provisioned vm = %+v, want running with id", vm)
	}
	assertKVMState(t, rt, vm.ID, agent.StateRunning)

	if err := rt.Stop(ctx, vm.ID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	assertKVMState(t, rt, vm.ID, agent.StateStopped)

	if _, err := rt.Start(ctx, vm.ID); err != nil {
		t.Fatalf("start: %v", err)
	}
	assertKVMState(t, rt, vm.ID, agent.StateRunning)

	if err := rt.Deprovision(ctx, vm.ID); err != nil {
		t.Fatalf("deprovision: %v", err)
	}
	assertKVMState(t, rt, vm.ID, agent.StateMissing)
}

func assertKVMState(t *testing.T, rt *Runtime, vmID, want string) {
	t.Helper()
	// Boot/shutdown are asynchronous; poll briefly for the expected state.
	deadline := time.Now().Add(15 * time.Second)
	for {
		vm, err := rt.Status(context.Background(), vmID)
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		if vm.State == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("state = %q, want %q", vm.State, want)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
