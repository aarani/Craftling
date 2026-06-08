// Package squashfs writes squashfs 4.0 filesystem images.
//
// This is a write-only implementation. The intended consumer is the
// Linux kernel's squashfs driver, which is the canonical reader; we
// do not implement reading. The intended producer is craftling's
// image pipeline (OCI layer tar -> rootfs image; see internal/image),
// where the input is a validated, bounded entry stream and the output
// is mounted read-only inside a Firecracker microVM.
//
// # Scope
//
// What this package implements:
//
//   - Squashfs 4.0 (the only format version since 2009 — kernel 2.6.29+).
//   - Regular files, directories, symlinks, hardlinks, character and
//     block device nodes, FIFOs and sockets.
//   - One pluggable compressor for data and metadata blocks (see
//     [Compressor]); the package itself does not import a compression
//     library so callers pick their dependency.
//   - UID/GID dedup table.
//
// What this package explicitly skips for v1:
//
//   - Fragment blocks (small-file tail packing). Costs ~5–15% space on
//     toolchain images, zero correctness. Easy to add later — the data
//     block writer is the same code path with a different placement
//     rule.
//   - Extended attributes. OCI images occasionally carry file
//     capabilities via xattrs; the v1 writer has no API to attach
//     them. Add a follow-up Create*WithXattrs variant when a real
//     image needs it.
//   - Export table (NFS file-handle lookups). Irrelevant for our
//     mount-and-exec consumption pattern.
//   - Reading. The kernel reads; the e2e test mounts and execs.
//
// # API shape
//
// The [Writer] exposes one method per entry type:
// [Writer.CreateFile], [Writer.CreateDir], [Writer.CreateSymlink],
// [Writer.CreateHardlink], [Writer.CreateCharDevice],
// [Writer.CreateBlockDevice], [Writer.CreateFIFO],
// [Writer.CreateSocket]. This keeps each entry shape statically
// typed — a symlink without a target or a device without a major
// number is unrepresentable at the call site, not a runtime
// validation error.
//
// [Writer.CreateFile] returns an [io.Writer] for streaming the
// file's contents; the returned writer is invalidated by the next
// Create* call or by Close. [Writer.Close] flushes the metadata
// tables and backpatches the superblock and MUST be called for the
// image to be valid.
//
// Entries may arrive in any order with one exception:
// [Writer.CreateHardlink] requires the target file to have been
// created earlier in the Writer's lifetime so the writer can
// resolve the target's inode number. Most OCI tars already satisfy
// this; the validating extractor on top of this package should
// enforce it explicitly.
package squashfs
