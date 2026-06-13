package image

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/aarani/craftling-go/internal/runspec"
)

// TestEnsure_cacheHit verifies Ensure returns a cached rootfs + its
// sidecar run spec without pulling, when both are already on disk. A
// real pull needs a registry; this exercises the hot path (every boot
// after the first) deterministically and offline.
func TestEnsure_cacheHit(t *testing.T) {
	dir := t.TempDir()
	s := &Store{CacheDir: dir}
	const digest = "sha256:cafebabe"

	// Seed a fake rootfs and its run-spec sidecar for the digest.
	rootfsPath, err := s.PathFor(digest)
	if err != nil {
		t.Fatalf("PathFor: %v", err)
	}
	if err := os.WriteFile(rootfsPath, []byte("squashfs"), 0o644); err != nil {
		t.Fatal(err)
	}
	want := runspec.RunSpec{
		Entrypoint: []string{"/usr/bin/java", "-jar"},
		Cmd:        []string{"server.jar"},
		Env:        []string{"EULA=true"},
		WorkingDir: "/data",
	}
	if err := s.writeRunSpecSidecar(digest, want); err != nil {
		t.Fatalf("writeRunSpecSidecar: %v", err)
	}

	// Ensure with a non-empty digest must not touch the network; it
	// returns the seeded path and spec. (A registry pull would fail in
	// this offline test, so success proves the cache-hit path.)
	gotPath, gotSpec, err := s.Ensure(context.Background(), "example.com/img:tag", digest)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if gotPath != rootfsPath {
		t.Errorf("path = %q, want %q", gotPath, rootfsPath)
	}
	if !reflect.DeepEqual(gotSpec, want) {
		t.Errorf("spec = %+v, want %+v", gotSpec, want)
	}
}

func TestRunSpecSidecar_roundTrip(t *testing.T) {
	dir := t.TempDir()
	s := &Store{CacheDir: dir}
	const digest = "sha256:deadbeef"

	spec := runspec.RunSpec{Cmd: []string{"run"}, Env: []string{"A=1"}}
	if err := s.writeRunSpecSidecar(digest, spec); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := s.readRunSpecSidecar(digest)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !reflect.DeepEqual(got, spec) {
		t.Errorf("round-trip = %+v, want %+v", got, spec)
	}

	// The sidecar lives next to the rootfs and is not itself a rootfs, so
	// GetExistingImages must ignore it.
	digests, err := s.GetExistingImages(context.Background())
	if err != nil {
		t.Fatalf("GetExistingImages: %v", err)
	}
	if len(digests) != 0 {
		t.Errorf("GetExistingImages counted sidecar(s): %v", digests)
	}
}

func TestUntagImage_removesSidecar(t *testing.T) {
	dir := t.TempDir()
	s := &Store{CacheDir: dir}
	const digest = "sha256:abc123"

	rootfsPath, _ := s.PathFor(digest)
	if err := os.WriteFile(rootfsPath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.writeRunSpecSidecar(digest, runspec.RunSpec{Cmd: []string{"c"}}); err != nil {
		t.Fatal(err)
	}
	sidecar, _ := s.runspecPathFor(digest)

	if err := s.UntagImage(context.Background(), digest); err != nil {
		t.Fatalf("UntagImage: %v", err)
	}
	for _, p := range []string{rootfsPath, sidecar} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s still present after UntagImage (err=%v)", filepath.Base(p), err)
		}
	}
}

func TestPinnedRef(t *testing.T) {
	const digest = "sha256:abcd"
	cases := map[string]string{
		"docker.io/library/busybox:1.37": "docker.io/library/busybox@" + digest,
		"docker.io/library/busybox":      "docker.io/library/busybox@" + digest,
		"ghcr.io/o/r@sha256:0000":        "ghcr.io/o/r@" + digest,
		"registry:5000/o/r:tag":          "registry:5000/o/r@" + digest, // port colon must not be mistaken for a tag
	}
	for in, want := range cases {
		if got := pinnedRef(in, digest); got != want {
			t.Errorf("pinnedRef(%q) = %q, want %q", in, got, want)
		}
	}
}
