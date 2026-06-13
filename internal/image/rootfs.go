// Package image converts OCI/docker images into squashfs rootfs files
// that boot as Firecracker microVMs. It is the craftling-go analogue
// of hpcc's rootfs store: pull a user image, flatten its layers, and
// seal them into a read-only squashfs image with a Go init binary
// injected as PID 1.
//
// On disk each prepared artifact is one regular file under CacheDir
// named "<algo>-<hex>.sqsh" (e.g. "sha256-abc123…ef.sqsh"). Colons in
// the image digest aren't portable in filenames, so we encode them as
// a dash and reverse the mapping when listing. The ".sqsh" suffix is
// part of the contract — it lets GetExistingImages skip strays
// without parsing them.
//
// PullImage builds the prepared rootfs in three streaming stages:
// (1) crane.Pull fetches the user image and we check its digest
// matches expectedDigest. (2) mutate.Extract gives us a flattened
// tarball with whiteouts already applied. (3) we stream the tarball
// through the in-tree squashfs writer (internal/squashfs), mapping
// each tar entry to a squashfs Create* call. No staging directory on
// the host; no shell-outs to `tar` or `mkfs.*`; no temp spool for the
// layer tar.
//
// The init injection and standard-mountpoint setup happen inline
// against the same squashfs writer — /.craftling/init is written from
// the bytes the caller has on hand, and /proc /sys /dev /tmp /run are
// created (if the user's tar didn't already include them) so the
// agent's setupInit can mount tmpfses on top of them without first
// needing to mkdir on a read-only rootfs.
//
// The OCI run config (entrypoint, cmd, env, workdir) is NOT baked into
// the rootfs. PullImage returns it as a runspec.RunSpec so the host can
// publish it into the microVM's MMDS at boot; the init agent fetches it
// back from MMDS rather than reading a file. This keeps the rootfs
// immutable and lets one image boot with different commands.
//
// Why squashfs rather than ext4? Read-only by design, naturally
// streaming-writable from a tar without a staging dir, and the host
// has no GPL e2fsprogs / squashfs-tools shell-out in the hot path.
// The kernel mounts the result read-only as /dev/vda inside the guest.
package image

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/aarani/craftling-go/internal/runspec"
	"github.com/aarani/craftling-go/internal/squashfs"
	"github.com/google/go-containerregistry/pkg/crane"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
)

// rootfsSuffix is appended to every prepared file. See package doc.
const rootfsSuffix = ".sqsh"

// standardMountpoints are pre-created (with default perms, if the
// user's tar didn't already include them) so the in-VM init agent's
// setupInit can mount kernel filesystems and tmpfses without first
// needing to mkdir on a read-only rootfs.
var standardMountpoints = []string{"/proc", "/sys", "/dev", "/tmp", "/run"}

// Tar-bomb caps. The OCI layer tar is attacker-controlled: a hostile
// image can describe an arbitrarily large logical filesystem in either
// total bytes or entry count, and the squashfs writer holds inode
// metadata in memory until Close. The caps below cut both off before
// the process gets to that point. Values are deliberately loose — the
// point is to prevent unbounded growth, not to police image size.
//
// var rather than const so tests can scale them down without
// generating gigabyte fixtures; production code never writes to them.
var (
	maxTarTotalBytes int64 = 16 << 30 // 16 GiB of logical file content
	maxTarEntryCount int64 = 1 << 20  // ~1M entries
)

// ErrTarTotalBytesExceeded is returned when streamTarToSquashfs sees
// more logical file content than maxTarTotalBytes — either a single
// header declares a size past the cap, or cumulative copies cross it.
var ErrTarTotalBytesExceeded = errors.New("image: tar total bytes exceeded cap")

// ErrTarEntryCountExceeded is returned when streamTarToSquashfs sees
// more tar entries than maxTarEntryCount.
var ErrTarEntryCountExceeded = errors.New("image: tar entry count exceeded cap")

// InitBinaries lists the host filesystem paths of the per-arch init
// binaries the caller has built (see cmd/init). The Store reads from
// these when laying down the init agent inside the rootfs at
// /.craftling/init before the squashfs image is sealed.
type InitBinaries struct {
	LinuxAmd64 string
	LinuxArm64 string
}

// Store converts OCI images into squashfs rootfs files on an on-disk
// cache. The zero value is not valid: CacheDir must be a writable
// directory, and Init must cover every architecture for which images
// will be pulled.
type Store struct {
	// CacheDir is where prepared rootfs files live. One file per
	// image digest; the file path is the artifact handed to the
	// Firecracker driver as /dev/vda.
	CacheDir string

	// Init is the per-arch init binaries to inject as PID 1 inside
	// the prepared rootfs.
	Init InitBinaries
}

// GetExistingImages enumerates the digests of every prepared rootfs
// file in CacheDir. Files whose name doesn't match the
// "<algo>-<hex>.sqsh" shape are skipped — operators sometimes stage
// scratch files or the in-progress output of a crashed build alongside
// finished artifacts.
//
// A missing CacheDir is not an error: a fresh host hasn't prepared
// anything yet, and the empty result is the right answer.
func (s *Store) GetExistingImages(_ context.Context) ([]string, error) {
	entries, err := os.ReadDir(s.CacheDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read cache dir %q: %w", s.CacheDir, err)
	}

	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		digest, ok := decodeRootfsName(e.Name())
		if !ok {
			continue
		}
		out = append(out, digest)
	}
	return out, nil
}

// PullImage pulls imagePath, verifies it matches expectedDigest,
// streams its flattened layer tar through the squashfs writer
// (injecting /.craftling/init and standard mountpoints inline), and
// publishes the result as CacheDir/<algo>-<hex>.sqsh.
//
// It returns the run spec distilled from the image's OCI config. The
// caller is responsible for delivering it to the guest via MMDS at boot
// (see internal/agent/firecracker); it is intentionally not baked into
// the rootfs.
//
// The build runs against a "<final>.tmp" sibling and atomically
// renames into place on success — partial files left behind by a crash
// never look like a finished artifact to GetExistingImages.
func (s *Store) PullImage(_ context.Context, imagePath, expectedDigest string) (runspec.RunSpec, error) {
	finalName, err := encodeRootfsName(expectedDigest)
	if err != nil {
		return runspec.RunSpec{}, err
	}
	if err := os.MkdirAll(s.CacheDir, 0o755); err != nil {
		return runspec.RunSpec{}, fmt.Errorf("create cache dir %q: %w", s.CacheDir, err)
	}
	finalPath := filepath.Join(s.CacheDir, finalName)
	tmpPath := finalPath + ".tmp"
	_ = os.Remove(tmpPath)

	img, err := crane.Pull(imagePath)
	if err != nil {
		return runspec.RunSpec{}, fmt.Errorf("pull %q: %w", imagePath, err)
	}
	dgst, err := img.Digest()
	if err != nil {
		return runspec.RunSpec{}, fmt.Errorf("read pulled digest: %w", err)
	}
	if dgst.String() != expectedDigest {
		return runspec.RunSpec{}, fmt.Errorf("image digest mismatch: expected %s, got %s",
			expectedDigest, dgst.String())
	}

	cfg, err := img.ConfigFile()
	if err != nil {
		return runspec.RunSpec{}, fmt.Errorf("read image config: %w", err)
	}
	if cfg.OS != "linux" {
		return runspec.RunSpec{}, fmt.Errorf("image: image OS %q is not linux", cfg.OS)
	}

	initBin, err := s.loadInitBinary(cfg.Architecture)
	if err != nil {
		return runspec.RunSpec{}, err
	}

	flat := mutate.Extract(img)
	defer flat.Close()

	if err := buildSquashfs(flat, initBin, tmpPath); err != nil {
		_ = os.Remove(tmpPath)
		return runspec.RunSpec{}, err
	}

	if err := os.Rename(tmpPath, finalPath); err != nil {
		_ = os.Remove(tmpPath)
		return runspec.RunSpec{}, fmt.Errorf("publish rootfs %q: %w", finalPath, err)
	}

	// Record the OCI-derived run spec next to the rootfs (a host-side
	// sidecar, not baked into the image) so a later boot can reconstruct
	// the MMDS payload from the cache without re-pulling. See Ensure.
	spec := specFromConfig(cfg)
	if err := s.writeRunSpecSidecar(expectedDigest, spec); err != nil {
		return runspec.RunSpec{}, err
	}
	return spec, nil
}

// Ensure returns the prepared squashfs rootfs path and the OCI-derived
// run spec for ref@digest, building it (pull + convert) only when it is
// not already cached. digest is the expected manifest digest — both the
// integrity check for the pull and the content-addressed cache key; pass
// the empty string to resolve it from ref first (linux/amd64).
//
// This is the Firecracker driver's entry point: it turns an image
// reference into a bootable read-only rootfs plus the spec to publish
// over MMDS, reusing the cache across VMs that share a digest.
func (s *Store) Ensure(ctx context.Context, ref, digest string) (rootfsPath string, spec runspec.RunSpec, err error) {
	if ref == "" {
		return "", runspec.RunSpec{}, errors.New("image: empty image reference")
	}
	if digest == "" {
		if digest, err = ResolveDigest(ctx, ref); err != nil {
			return "", runspec.RunSpec{}, err
		}
	}
	rootfsPath, err = s.PathFor(digest)
	if err != nil {
		return "", runspec.RunSpec{}, err
	}
	if fileExists(rootfsPath) {
		if spec, err = s.readRunSpecSidecar(digest); err == nil {
			return rootfsPath, spec, nil
		}
		// Sidecar missing or unreadable (older build, partial write): fall
		// through and rebuild so the spec is regenerated with the rootfs.
	}
	if spec, err = s.PullImage(ctx, pinnedRef(ref, digest), digest); err != nil {
		return "", runspec.RunSpec{}, err
	}
	return rootfsPath, spec, nil
}

// ResolveDigest resolves ref to its linux/amd64 manifest digest.
// Firecracker hosts are amd64; multi-arch host selection is a follow-up.
func ResolveDigest(ctx context.Context, ref string) (string, error) {
	d, err := crane.Digest(ref,
		crane.WithContext(ctx),
		crane.WithPlatform(&v1.Platform{OS: "linux", Architecture: "amd64"}))
	if err != nil {
		return "", fmt.Errorf("resolve digest for %q: %w", ref, err)
	}
	return d, nil
}

// PathFor returns the absolute on-disk path of the prepared rootfs for
// digest, whether or not the file has been built yet. The Firecracker
// runtime calls this to find the squashfs image to attach as /dev/vda.
func (s *Store) PathFor(digest string) (string, error) {
	name, err := encodeRootfsName(digest)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.CacheDir, name), nil
}

// runspecSuffix names the host-side sidecar that records an image's
// OCI-derived run spec alongside its <algo>-<hex>.sqsh rootfs.
const runspecSuffix = ".runspec.json"

// runspecPathFor returns the sidecar path for digest's rootfs.
func (s *Store) runspecPathFor(digest string) (string, error) {
	name, err := encodeRootfsName(digest)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.CacheDir, strings.TrimSuffix(name, rootfsSuffix)+runspecSuffix), nil
}

// writeRunSpecSidecar atomically persists spec next to digest's rootfs.
func (s *Store) writeRunSpecSidecar(digest string, spec runspec.RunSpec) error {
	p, err := s.runspecPathFor(digest)
	if err != nil {
		return err
	}
	data, err := json.Marshal(spec)
	if err != nil {
		return fmt.Errorf("marshal run spec sidecar: %w", err)
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write run spec sidecar: %w", err)
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("publish run spec sidecar: %w", err)
	}
	return nil
}

// readRunSpecSidecar loads the run spec recorded for digest's rootfs.
func (s *Store) readRunSpecSidecar(digest string) (runspec.RunSpec, error) {
	p, err := s.runspecPathFor(digest)
	if err != nil {
		return runspec.RunSpec{}, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return runspec.RunSpec{}, err
	}
	var spec runspec.RunSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		return runspec.RunSpec{}, fmt.Errorf("parse run spec sidecar %q: %w", p, err)
	}
	return spec, nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// pinnedRef rewrites ref to pin it to digest (<repo>@<digest>) so the
// pull resolves the exact manifest the cache is keyed by, dropping any
// tag or existing digest.
func pinnedRef(ref, digest string) string {
	base := ref
	if i := strings.IndexByte(ref, '@'); i >= 0 {
		base = ref[:i]
	} else if i := strings.LastIndexByte(ref, ':'); i > strings.LastIndexByte(ref, '/') {
		base = ref[:i]
	}
	return base + "@" + digest
}

// UntagImage removes the prepared rootfs for digest. A missing file is
// not an error — eviction can race against a crash that left no
// artifact behind, so the caller treats this as best-effort cleanup.
func (s *Store) UntagImage(_ context.Context, digest string) error {
	name, err := encodeRootfsName(digest)
	if err != nil {
		return err
	}
	p := filepath.Join(s.CacheDir, name)
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove rootfs %q: %w", p, err)
	}
	// Drop the run-spec sidecar too so it doesn't outlive its rootfs.
	if sp, err := s.runspecPathFor(digest); err == nil {
		if err := os.Remove(sp); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove run spec sidecar %q: %w", sp, err)
		}
	}
	return nil
}

// loadInitBinary returns the bytes of the init agent for arch.
func (s *Store) loadInitBinary(arch string) ([]byte, error) {
	var p string
	switch arch {
	case "amd64":
		p = s.Init.LinuxAmd64
	case "arm64":
		p = s.Init.LinuxArm64
	default:
		return nil, fmt.Errorf("no init binary configured for linux/%s", arch)
	}
	if p == "" {
		return nil, fmt.Errorf("init binary path for linux/%s is not set", arch)
	}
	bin, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("read init binary %q: %w", p, err)
	}
	return bin, nil
}

// specFromConfig distills the OCI image config into the RunSpec the
// init agent reads at boot.
func specFromConfig(cfg *v1.ConfigFile) runspec.RunSpec {
	return runspec.RunSpec{
		Entrypoint: append([]string(nil), cfg.Config.Entrypoint...),
		Cmd:        append([]string(nil), cfg.Config.Cmd...),
		Env:        append([]string(nil), cfg.Config.Env...),
		WorkingDir: cfg.Config.WorkingDir,
	}
}

// buildSquashfs streams tarStream into a fresh squashfs image at
// outPath, injects the init binary at /.craftling/init, and ensures the
// standard guest mountpoints exist. The caller is responsible for
// renaming outPath into place on success.
//
// The compressor is gzip (stdlib compress/zlib under the hood; the
// squashfs "gzip" ID expects a zlib stream, not a gzip-with-header
// stream). Gzip support is the most universally compiled-in squashfs
// decompressor across Linux kernel builds, including the minimal
// Firecracker reference kernels.
func buildSquashfs(tarStream io.Reader, initBin []byte, outPath string) error {
	out, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("create rootfs image %q: %w", outPath, err)
	}
	closeOut := func(retErr error) error {
		if cerr := out.Close(); cerr != nil && retErr == nil {
			return fmt.Errorf("close rootfs image %q: %w", outPath, cerr)
		}
		return retErr
	}

	w, err := squashfs.NewWriter(out, squashfs.WithCompressor(squashfs.GzipCompressor{}))
	if err != nil {
		return closeOut(fmt.Errorf("init squashfs writer: %w", err))
	}

	created := map[string]bool{}

	if err := streamTarToSquashfs(w, tar.NewReader(tarStream), created); err != nil {
		return closeOut(fmt.Errorf("stream layer tar: %w", err))
	}
	if err := injectInit(w, initBin, created); err != nil {
		return closeOut(fmt.Errorf("inject init: %w", err))
	}
	if err := ensureStandardMountpoints(w, created); err != nil {
		return closeOut(fmt.Errorf("create mountpoints: %w", err))
	}
	if err := w.Close(); err != nil {
		return closeOut(fmt.Errorf("seal squashfs: %w", err))
	}
	return closeOut(nil)
}

// streamTarToSquashfs iterates the OCI layer tar one entry at a time
// and translates each into a squashfs Create* call. The translation is
// intentionally strict: any path that fails normalization (".."
// segments, absolute paths inside the archive, empty names, NUL bytes)
// terminates the build — attacker-controlled OCI bytes flow through
// this loop, so permissiveness here is the wrong default. Duplicate
// entries for the same path keep the first (squashfs has no replace
// semantics).
//
// Entries under /.craftling are dropped — that namespace belongs to
// the pipeline, and injectInit writes it fresh after the stream
// completes.
func streamTarToSquashfs(w *squashfs.Writer, tr *tar.Reader, created map[string]bool) error {
	var (
		entryCount int64
		totalBytes int64
	)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read tar header: %w", err)
		}

		entryCount++
		if entryCount > maxTarEntryCount {
			return fmt.Errorf("%w: %d entries past cap %d",
				ErrTarEntryCountExceeded, entryCount, maxTarEntryCount)
		}
		// Reject up front on a header-declared size that's already past
		// the cap, before we start copying — covers a single fat entry
		// without needing to read it.
		if h.Size > 0 {
			if h.Size > maxTarTotalBytes-totalBytes {
				return fmt.Errorf("%w: header declares %d bytes, %d already used, cap %d",
					ErrTarTotalBytesExceeded, h.Size, totalBytes, maxTarTotalBytes)
			}
		}

		p, ok := normalizeTarPath(h.Name)
		if !ok {
			return fmt.Errorf("rejected tar entry name %q", h.Name)
		}
		if p == "" {
			// Tar archive root ("./" or "/") — squashfs has an implicit
			// root, nothing to do.
			continue
		}
		if p == runspec.InstallDir || strings.HasPrefix(p, runspec.InstallDir+"/") {
			// Pipeline-owned namespace. Drop whatever the user shipped;
			// injectInit writes ours.
			continue
		}
		if created[p] {
			continue
		}
		if err := ensureAncestors(w, p, created); err != nil {
			return err
		}

		attrs := squashfs.Attrs{
			Path:  p,
			Mode:  fs.FileMode(h.Mode) & fs.ModePerm,
			UID:   uint32(h.Uid),
			GID:   uint32(h.Gid),
			Mtime: h.ModTime,
		}

		switch h.Typeflag {
		case tar.TypeReg, tar.TypeRegA:
			fw, err := w.CreateFile(attrs)
			if err != nil {
				return fmt.Errorf("create file %q: %w", p, err)
			}
			// Cap how many bytes we'll copy from this entry even if the
			// header lied about Size. LimitReader stops one past the
			// remaining budget so we can distinguish "ran out" from
			// "fit cleanly".
			remaining := maxTarTotalBytes - totalBytes
			n, err := io.Copy(fw, io.LimitReader(tr, remaining+1))
			if err != nil {
				return fmt.Errorf("write file %q: %w", p, err)
			}
			if n > remaining {
				return fmt.Errorf("%w: entry %q exceeded remaining budget %d",
					ErrTarTotalBytesExceeded, p, remaining)
			}
			totalBytes += n
		case tar.TypeDir:
			if err := w.CreateDir(attrs); err != nil {
				return fmt.Errorf("create dir %q: %w", p, err)
			}
		case tar.TypeSymlink:
			if h.Linkname == "" {
				return fmt.Errorf("symlink %q has empty target", p)
			}
			if err := w.CreateSymlink(attrs, h.Linkname); err != nil {
				return fmt.Errorf("create symlink %q: %w", p, err)
			}
		case tar.TypeLink:
			target, ok := normalizeTarPath(h.Linkname)
			if !ok || target == "" {
				return fmt.Errorf("hardlink %q has invalid target %q", p, h.Linkname)
			}
			if err := w.CreateHardlink(p, target); err != nil {
				return fmt.Errorf("create hardlink %q -> %q: %w", p, target, err)
			}
		case tar.TypeChar:
			if err := w.CreateCharDevice(attrs, uint32(h.Devmajor), uint32(h.Devminor)); err != nil {
				return fmt.Errorf("create char device %q: %w", p, err)
			}
		case tar.TypeBlock:
			if err := w.CreateBlockDevice(attrs, uint32(h.Devmajor), uint32(h.Devminor)); err != nil {
				return fmt.Errorf("create block device %q: %w", p, err)
			}
		case tar.TypeFifo:
			if err := w.CreateFIFO(attrs); err != nil {
				return fmt.Errorf("create fifo %q: %w", p, err)
			}
		case tar.TypeXGlobalHeader, tar.TypeXHeader:
			// PAX metadata — archive/tar applies it to subsequent
			// entries before we see them. Nothing for us to do.
			continue
		default:
			// Unknown tar type. Skipping rather than erroring because
			// OCI tars occasionally carry vendor-specific markers we
			// don't need to interpret.
			continue
		}
		created[p] = true
	}
}

// injectInit writes the per-arch init binary to /.craftling/init inside
// the squashfs image, creating /.craftling itself first
// (streamTarToSquashfs always strips /.craftling, so it never exists at
// this point — but the idempotency keeps the function safe to reorder).
//
// The init binary is mode 0755 so the kernel's exec sees an executable,
// uid/gid 0 so it runs as root inside the guest, which it must —
// PID 1 mounts /proc, /sys, /dev/shm. The run spec is delivered out of
// band via MMDS (see package doc), not written here.
func injectInit(w *squashfs.Writer, initBin []byte, created map[string]bool) error {
	if !created[runspec.InstallDir] {
		if err := w.CreateDir(squashfs.Attrs{
			Path: runspec.InstallDir,
			Mode: 0o755,
		}); err != nil {
			return fmt.Errorf("create %s: %w", runspec.InstallDir, err)
		}
		created[runspec.InstallDir] = true
	}

	fw, err := w.CreateFile(squashfs.Attrs{
		Path: runspec.InitPath,
		Mode: 0o755,
	})
	if err != nil {
		return fmt.Errorf("create %s: %w", runspec.InitPath, err)
	}
	if _, err := io.Copy(fw, bytes.NewReader(initBin)); err != nil {
		return fmt.Errorf("write %s: %w", runspec.InitPath, err)
	}
	created[runspec.InitPath] = true
	return nil
}

// ensureStandardMountpoints creates the kernel-filesystem and tmpfs
// mountpoint directories the in-VM init agent expects. If the user's
// image already shipped one, we keep their entry — the agent shadows it
// with a mount immediately anyway.
func ensureStandardMountpoints(w *squashfs.Writer, created map[string]bool) error {
	for _, p := range standardMountpoints {
		if created[p] {
			continue
		}
		mode := fs.FileMode(0o755)
		if p == "/tmp" {
			mode = 0o1777
		}
		if err := w.CreateDir(squashfs.Attrs{Path: p, Mode: mode}); err != nil {
			return fmt.Errorf("create %s: %w", p, err)
		}
		created[p] = true
	}
	return nil
}

// ensureAncestors walks p's ancestor directories from shallowest to
// deepest, calling CreateDir for any that haven't been created yet. OCI
// tars sometimes elide explicit dir entries for ancestors on the
// assumption that the extractor will mkdir -p; the squashfs writer
// needs the directory tree explicit by Close. Default ancestor perms
// are 0755 owned by root — matching what `tar -xpf` would do.
func ensureAncestors(w *squashfs.Writer, p string, created map[string]bool) error {
	parent := path.Dir(p)
	if parent == "/" || parent == "." {
		return nil
	}
	// Build ancestors from shallowest to deepest so children of
	// ancestors don't get attempted before the ancestors exist.
	var ancestors []string
	for cur := parent; cur != "/" && cur != "."; cur = path.Dir(cur) {
		ancestors = append([]string{cur}, ancestors...)
	}
	for _, a := range ancestors {
		if created[a] {
			continue
		}
		if err := w.CreateDir(squashfs.Attrs{
			Path: a,
			Mode: 0o755,
		}); err != nil {
			return fmt.Errorf("create implicit ancestor %q for %q: %w", a, p, err)
		}
		created[a] = true
	}
	return nil
}

// normalizeTarPath maps a tar Header.Name into the canonical absolute
// form the squashfs writer expects ("/foo/bar"), or reports ok=false
// for anything we refuse to admit: empty names, NUL bytes, paths
// containing ".." or "." components, absolute paths (tar-escape
// attempts).
//
// The "./" prefix and trailing "/" common to dir entries are stripped.
// The bare archive root ("." or "/") returns ("", true) so callers can
// skip it cleanly without a name error.
func normalizeTarPath(name string) (string, bool) {
	if name == "" || strings.ContainsRune(name, 0) {
		return "", false
	}
	if name == "." || name == "/" || name == "./" {
		return "", true
	}
	name = strings.TrimPrefix(name, "./")
	if strings.HasPrefix(name, "/") {
		// Reject absolute paths inside the archive — a well-formed OCI
		// layer tar never carries them, and `tar -xpf` historically
		// treats them as a path-traversal attempt.
		return "", false
	}
	name = strings.TrimSuffix(name, "/")
	if name == "" {
		return "", false
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", false
		}
	}
	return "/" + name, true
}

// encodeRootfsName turns an image digest ("sha256:abc…", or bare hex
// which we assume is sha256) into the on-disk filename. Rejects empty
// input and digests whose hex part is empty.
func encodeRootfsName(digest string) (string, error) {
	algo, hex, ok := splitDigest(digest)
	if !ok {
		return "", fmt.Errorf("invalid image digest %q", digest)
	}
	return algo + "-" + hex + rootfsSuffix, nil
}

// decodeRootfsName is the reverse of encodeRootfsName. Returns
// ok=false for anything that doesn't match the shape, so the caller can
// skip strays.
func decodeRootfsName(name string) (digest string, ok bool) {
	if !strings.HasSuffix(name, rootfsSuffix) {
		return "", false
	}
	stem := strings.TrimSuffix(name, rootfsSuffix)
	dash := strings.IndexByte(stem, '-')
	if dash <= 0 || dash == len(stem)-1 {
		return "", false
	}
	return stem[:dash] + ":" + stem[dash+1:], true
}

func splitDigest(s string) (algo, hex string, ok bool) {
	if s == "" {
		return "", "", false
	}
	if i := strings.IndexByte(s, ':'); i >= 0 {
		if i == 0 || i == len(s)-1 {
			return "", "", false
		}
		return s[:i], s[i+1:], true
	}
	// No algo prefix: assume sha256.
	return "sha256", s, true
}
