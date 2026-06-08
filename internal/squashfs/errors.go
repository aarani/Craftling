package squashfs

import "errors"

// ErrIncompressible is the sentinel a Compressor returns when the
// compressed output would not be smaller than the input. The writer
// then stores the block raw with the squashfs "uncompressed" bit
// set. Returning this is not an error condition — it is the normal
// signal for "store raw."
var ErrIncompressible = errors.New("squashfs: block did not compress smaller; store raw")

// ErrClosed is returned by Writer methods called after Close.
var ErrClosed = errors.New("squashfs: writer is closed")

// ErrInvalidPath is returned for entry paths that aren't an absolute
// canonical path inside the image: empty string, missing leading /,
// trailing /, or any "." / ".." component. The writer does not
// resolve, normalize, or sanitize — that is the validating
// extractor's job upstream.
var ErrInvalidPath = errors.New("squashfs: entry path must be absolute and canonical")

// ErrHardlinkTarget is returned by CreateHardlink when target has
// not been created earlier in this Writer's lifetime, or refers to
// a non-file entry (directory, symlink, device, FIFO, socket).
// Squashfs hardlinks are inode references, so the target inode
// must already exist and be a regular file.
var ErrHardlinkTarget = errors.New("squashfs: hardlink target must be a regular file created earlier")

// ErrStaleEntry is returned when the caller writes to an io.Writer
// previously returned by CreateFile after a subsequent Create* call
// or Close has invalidated it. Capture and consume the writer
// before starting the next entry.
var ErrStaleEntry = errors.New("squashfs: write to a CreateFile writer invalidated by a later call")
