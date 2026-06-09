// Package storage is the durable home of game-server world data (P5b).
//
// On the squashfs+init image path a VM's rootfs is read-only and its
// writable world lives on a per-server ext4 "world disk" (see
// internal/agent/firecracker). That disk makes a world survive a stop/start on
// one host, but it is still local: deleting the VM, or moving a server to
// another host, would lose it. A WorldStore is the off-host system of record —
// an object store (S3) or shared filesystem (NFS) — that the agent snapshots
// the disk into and restores it from, so a world outlives any single VM or
// host.
//
// The agent owns the disk<->stream codec (gzip of the raw ext4 image); the
// store moves opaque bytes keyed by server id and knows nothing about their
// shape. That keeps backends trivial to add: a new WorldStore only has to
// stream bytes in and out.
package storage

import (
	"context"
	"errors"
	"io"
	"strings"
)

// WorldSuffix is appended to a SafeKey to name a stored snapshot. The contents
// are opaque to the store (the agent gzips a raw ext4 image into them); the
// suffix only marks the object/file as a world snapshot. Shared by both
// backends so their naming can't drift.
const WorldSuffix = ".world"

// ErrWorldNotFound is returned by Get when no snapshot is stored for a server.
// Callers use it to distinguish "this server has no saved world yet" (boot a
// fresh disk) from a real I/O failure.
var ErrWorldNotFound = errors.New("storage: world not found")

// WorldStore is the durable store of per-server world snapshots. A snapshot is
// an opaque byte stream the agent produces from a world disk; the store keys it
// by server id. Implementations must be safe for concurrent use by different
// servers (the agent runs many VMs at once); concurrent operations on the same
// server id are the caller's responsibility to avoid (the agent serializes a
// server's lifecycle).
type WorldStore interface {
	// Exists reports whether a snapshot is stored for serverID. The agent
	// checks this on Provision to decide between restoring and formatting fresh.
	Exists(ctx context.Context, serverID string) (bool, error)

	// Put stores (replacing any prior) the snapshot for serverID, reading the
	// stream to EOF. A failure must not leave a partial snapshot a later Get
	// would hand back as if whole.
	Put(ctx context.Context, serverID string, r io.Reader) error

	// Get opens the stored snapshot for serverID. The caller closes the
	// returned reader. Returns ErrWorldNotFound when nothing is stored.
	Get(ctx context.Context, serverID string) (io.ReadCloser, error)

	// Delete removes the snapshot for serverID. A missing snapshot is not an
	// error — delete is idempotent teardown.
	Delete(ctx context.Context, serverID string) error
}

// SafeKey maps a server id to a single safe object/path token, replacing
// anything outside [A-Za-z0-9._-] with '_' so an id can never traverse a
// filesystem path or smuggle separators into an object key. It mirrors the
// firecracker driver's disk-keying guard; an empty/all-unsafe id collapses to
// "_". Both the DirStore and S3 backends use it to derive their on-disk/object
// name from a server id.
func SafeKey(serverID string) string {
	var b strings.Builder
	for _, r := range serverID {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "_"
	}
	return b.String()
}
