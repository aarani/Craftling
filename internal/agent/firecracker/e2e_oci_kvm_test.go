//go:build kvm && linux

// End-to-end test for the OCI-image rootfs path: the agent builds a
// squashfs rootfs from a real docker image via internal/image and boots
// it, rather than a static per-version ext4 base. This is the path the
// "agent still uses static images" change wired up.
//
//	Runtime.Provision(VMSpec{Image, ImageDigest})
//	  → image.Store.Ensure pulls + converts the image to squashfs (cached)
//	  → firecracker boots it read-only with init=/.craftling/init
//	  → host publishes the image's OCI run spec over MMDS
//	  → in-VM init fetches the run spec and execs the image's command
//
// We assert the init logs (serial console → firecracker.log) show it
// started the image's command, which can only happen if the store-built
// rootfs booted and init read the image-derived run spec from MMDS.
//
// Shares prerequisites/helpers with e2e_mmds_kvm_test.go. Gated behind
// `kvm`; skips without /dev/kvm, root, a firecracker binary, FC_KERNEL,
// or registry access.
package firecracker

import (
	"context"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/aarani/craftling-go/internal/agent"
	"github.com/aarani/craftling-go/internal/image"
	"github.com/google/go-containerregistry/pkg/crane"
	v1 "github.com/google/go-containerregistry/pkg/v1"
)

func TestKVMOCIImageBoot(t *testing.T) {
	binPath, kernel := requireFirecracker(t)

	arch := runtime.GOARCH
	initBin := buildInitBinary(t, arch)
	store := &image.Store{CacheDir: t.TempDir()}
	switch arch {
	case "amd64":
		store.Init.LinuxAmd64 = initBin
	case "arm64":
		store.Init.LinuxArm64 = initBin
	default:
		t.Skipf("unsupported test arch %q", arch)
	}

	// Pin busybox to the host-arch manifest so the rootfs the store builds
	// matches the kernel we boot.
	digest, err := crane.Digest(e2eImageRef, crane.WithPlatform(&v1.Platform{OS: "linux", Architecture: arch}))
	if err != nil {
		t.Fatalf("resolve %s digest: %v", e2eImageRef, err)
	}

	rt, err := New(Config{
		BinaryPath:    binPath,
		KernelPath:    kernel,
		WorkDir:       t.TempDir(),
		ImageStore:    store, // OCI path; no ImageDir / static images at all
		AdvertiseHost: "127.0.0.1",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer rt.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	vm, err := rt.Provision(ctx, agent.VMSpec{
		ServerID:    "e2e-oci",
		Image:       e2eImageRef,
		ImageDigest: digest,
		CPUs:        1,
		MemoryMB:    256,
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	defer func() { _ = rt.Deprovision(context.Background(), vm.ID) }()

	if vm.State != agent.StateRunning {
		t.Fatalf("provisioned vm = %+v, want running", vm)
	}

	// The store must have produced a squashfs rootfs for the digest.
	if path, err := store.PathFor(digest); err != nil {
		t.Fatalf("PathFor: %v", err)
	} else if _, err := os.Stat(path); err != nil {
		t.Errorf("expected store to build rootfs at %s: %v", path, err)
	}

	consoleLog := findConsoleLog(t, rt.cfg.WorkDir)
	// init logs "workload started" once it has fetched the run spec from
	// MMDS and execed the image's command (busybox's /bin/sh).
	if !waitForConsole(t, consoleLog, "init: workload started", 120*time.Second) {
		t.Fatalf("did not observe init starting the image command within timeout\n--- console ---\n%s",
			tailFile(consoleLog, 4096))
	}
	t.Logf("OCI image %s booted from store-built squashfs and init execed its command", e2eImageRef)
}
