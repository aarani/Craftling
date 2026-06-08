package firecracker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/aarani/craftling-go/internal/agent"
)

// newTestRuntime builds a Runtime over throwaway host artifacts so the
// non-KVM unit tests can exercise validation, image resolution, and the
// no-process idempotency edges without ever launching Firecracker.
func newTestRuntime(t *testing.T) *Runtime {
	t.Helper()
	dir := t.TempDir()
	kernel := filepath.Join(dir, "vmlinux")
	if err := os.WriteFile(kernel, []byte("kernel"), 0o600); err != nil {
		t.Fatalf("write kernel: %v", err)
	}
	imageDir := filepath.Join(dir, "images")
	if err := os.MkdirAll(imageDir, 0o750); err != nil {
		t.Fatalf("mkdir images: %v", err)
	}
	rt, err := New(Config{
		KernelPath:    kernel,
		ImageDir:      imageDir,
		WorkDir:       filepath.Join(dir, "work"),
		AdvertiseHost: "10.0.0.5",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return rt
}

func TestNewValidatesArtifacts(t *testing.T) {
	dir := t.TempDir()
	kernel := filepath.Join(dir, "vmlinux")
	if err := os.WriteFile(kernel, []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := New(Config{ImageDir: dir}); err == nil {
		t.Error("missing kernel: expected error")
	}
	if _, err := New(Config{KernelPath: filepath.Join(dir, "nope")}); err == nil {
		t.Error("nonexistent kernel: expected error")
	}
	if _, err := New(Config{KernelPath: kernel, ImageDir: filepath.Join(dir, "nope")}); err == nil {
		t.Error("missing image dir: expected error")
	}
	if _, err := New(Config{KernelPath: kernel, ImageDir: kernel}); err == nil {
		t.Error("image dir is a file: expected error")
	}
}

func TestImageFor(t *testing.T) {
	dir := t.TempDir()
	mustTouch(t, filepath.Join(dir, "minecraft-1.20.4.ext4"))
	mustTouch(t, filepath.Join(dir, "base.ext4"))

	cfg := Config{ImageDir: dir, DefaultImage: "base.ext4"}

	got, err := cfg.imageFor("1.20.4")
	if err != nil || filepath.Base(got) != "minecraft-1.20.4.ext4" {
		t.Errorf("imageFor(known) = %q, %v; want the versioned image", got, err)
	}

	got, err = cfg.imageFor("9.9.9")
	if err != nil || filepath.Base(got) != "base.ext4" {
		t.Errorf("imageFor(unknown) = %q, %v; want the default image", got, err)
	}

	noDefault := Config{ImageDir: dir}
	if _, err := noDefault.imageFor("9.9.9"); err == nil {
		t.Error("imageFor(unknown, no default): expected error")
	}
}

func TestImageForMissingDefault(t *testing.T) {
	cfg := Config{ImageDir: t.TempDir(), DefaultImage: "ghost.ext4"}
	if _, err := cfg.imageFor("1.20.4"); err == nil {
		t.Error("default image absent on disk: expected error")
	}
}

// TestLifecycleIdempotencyNoProcess covers the contract edges the control plane
// relies on, none of which touch a real VM: operations on unknown ids.
func TestLifecycleIdempotencyNoProcess(t *testing.T) {
	rt := newTestRuntime(t)
	ctx := context.Background()

	if err := rt.Stop(ctx, "ghost"); err != nil {
		t.Errorf("stop unknown = %v, want nil (idempotent)", err)
	}
	if err := rt.Deprovision(ctx, "ghost"); err != nil {
		t.Errorf("deprovision unknown = %v, want nil (idempotent)", err)
	}
	if _, err := rt.Start(ctx, "ghost"); !errors.Is(err, agent.ErrVMNotFound) {
		t.Errorf("start unknown = %v, want ErrVMNotFound", err)
	}
	vm, err := rt.Status(ctx, "ghost")
	if err != nil {
		t.Fatalf("status unknown: %v", err)
	}
	if vm.State != agent.StateMissing {
		t.Errorf("status unknown state = %q, want missing", vm.State)
	}
}

func TestProvisionRejectsInvalidSpec(t *testing.T) {
	rt := newTestRuntime(t)
	ctx := context.Background()
	for _, spec := range []agent.VMSpec{
		{Version: "1.20.4", CPUs: 0, MemoryMB: 1024},
		{Version: "1.20.4", CPUs: 2, MemoryMB: 0},
	} {
		if _, err := rt.Provision(ctx, spec); err == nil {
			t.Errorf("Provision(%+v): expected error", spec)
		}
	}
}

func TestProvisionUnknownVersion(t *testing.T) {
	rt := newTestRuntime(t)
	if _, err := rt.Provision(context.Background(),
		agent.VMSpec{Version: "1.20.4", CPUs: 2, MemoryMB: 1024}); err == nil {
		t.Error("Provision with no matching image: expected error")
	}
}

func TestCopyFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "sub", "dst")
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		t.Fatal(err)
	}
	want := []byte("rootfs-bytes")
	if err := os.WriteFile(src, want, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(src, dst); err != nil {
		t.Fatalf("copyFile: %v", err)
	}
	got, err := os.ReadFile(dst) //nolint:gosec // test path
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("copied = %q, want %q", got, want)
	}
}

func mustTouch(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte{}, 0o600); err != nil {
		t.Fatalf("touch %s: %v", path, err)
	}
}
