package firecracker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aarani/craftling-go/internal/agent"
	"github.com/aarani/craftling-go/internal/image"
)

// fakeKernel writes a stand-in vmlinux file so Config.validate (which
// only stat's the path) passes without a real kernel.
func fakeKernel(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "vmlinux")
	if err := os.WriteFile(p, []byte("\x7fELF"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestConfigValidate_imageStoreSatisfiesRootfsSource(t *testing.T) {
	// An OCI-only host configures an ImageStore and no ImageDir.
	r, err := New(Config{
		KernelPath: fakeKernel(t),
		WorkDir:    t.TempDir(),
		ImageStore: &image.Store{CacheDir: t.TempDir()},
	})
	if err != nil {
		t.Fatalf("New with ImageStore and no ImageDir: %v", err)
	}
	r.Close()
}

func TestConfigValidate_requiresARootfsSource(t *testing.T) {
	// Neither ImageDir nor ImageStore: nothing can back a rootfs.
	_, err := New(Config{
		KernelPath: fakeKernel(t),
		WorkDir:    t.TempDir(),
	})
	if err == nil {
		t.Fatal("New with neither ImageDir nor ImageStore: want error")
	}
	if !strings.Contains(err.Error(), "ImageDir or ImageStore") {
		t.Errorf("error = %v, want it to mention ImageDir or ImageStore", err)
	}
}

func TestProvision_imageWithoutStoreFails(t *testing.T) {
	// Legacy ext4 host (ImageDir, no ImageStore) asked to boot an OCI image
	// must fail clearly rather than silently fall back to a static image.
	r, err := New(Config{
		KernelPath: fakeKernel(t),
		ImageDir:   t.TempDir(),
		WorkDir:    t.TempDir(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer r.Close()

	_, err = r.Provision(context.Background(), agent.VMSpec{
		ServerID: "s1",
		Image:    "docker.io/library/busybox:1.37",
		CPUs:     1,
		MemoryMB: 256,
	})
	if err == nil {
		t.Fatal("Provision with image but no store: want error")
	}
	if !strings.Contains(err.Error(), "no image store") {
		t.Errorf("error = %v, want it to mention the missing image store", err)
	}
}
