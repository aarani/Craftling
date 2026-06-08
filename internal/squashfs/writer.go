package squashfs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
	"time"
)

// Attrs is the metadata common to every entry that owns an inode of
// its own: ownership, permission bits, and modification time. Used
// by every Create* method except CreateHardlink (which inherits all
// metadata from its target inode).
//
// The writer takes ownership of an Attrs for the duration of the
// Create* call only; callers may reuse the value afterwards.
type Attrs struct {
	// Path is the entry's absolute path inside the image, with a
	// leading "/" and no trailing "/". Must be canonical: no ".",
	// "..", or empty components. The root directory ("/") is
	// implicit and must not be written explicitly.
	Path string

	// Mode carries the Unix permission bits (the low 12 bits:
	// rwx for u/g/o plus setuid/setgid/sticky). The type bits of
	// fs.FileMode are ignored — the entry type is determined by
	// which Create* method was called.
	Mode fs.FileMode

	// UID, GID are the numeric owner/group. The writer dedups
	// distinct values into the squashfs ID table; callers do not
	// need to dedup themselves.
	UID uint32
	GID uint32

	// Mtime is the entry's modification time. Squashfs stores
	// seconds since the Unix epoch as a uint32; the writer
	// truncates sub-second precision and rejects times outside
	// [1970-01-01, 2106-02-07]. If WithFixedMtime was supplied at
	// construction, that value overrides Mtime.
	Mtime time.Time
}

// Option configures a Writer at construction time. Pass options to
// NewWriter.
type Option func(*writerConfig) error

// writerConfig is the internal aggregate of caller-provided options.
// Kept unexported so the option-function shape stays stable even as
// the configurable surface grows.
type writerConfig struct {
	blockSize  uint32
	compressor Compressor
	mtime      time.Time // optional override applied to all entries; zero means use Attrs.Mtime
}

// WithBlockSize sets the data-block size for the image. Must be a
// power of two between MinBlockSize and MaxBlockSize inclusive. The
// default is DefaultBlockSize (128 KiB).
func WithBlockSize(n uint32) Option {
	return func(c *writerConfig) error {
		if n < MinBlockSize || n > MaxBlockSize {
			return errors.New("squashfs: block size out of range")
		}
		if n&(n-1) != 0 {
			return errors.New("squashfs: block size must be a power of two")
		}
		c.blockSize = n
		return nil
	}
}

// WithCompressor selects the compression backend. Required — there
// is no default, so callers must make a deliberate dependency choice.
func WithCompressor(c Compressor) Option {
	return func(cfg *writerConfig) error {
		if c == nil {
			return errors.New("squashfs: compressor is required")
		}
		cfg.compressor = c
		return nil
	}
}

// WithFixedMtime overrides every entry's Mtime with t. Useful for
// deterministic builds: when the image is keyed by content digest
// (as in craftling's rootfs cache), the source tarball's per-file mtimes
// otherwise leak into the digest and defeat caching.
func WithFixedMtime(t time.Time) Option {
	return func(c *writerConfig) error {
		c.mtime = t
		return nil
	}
}

// Writer streams a squashfs image to an underlying io.WriteSeeker.
// See package doc for the API contract.
type Writer struct {
	out io.WriteSeeker
	cfg writerConfig

	closed   bool
	finished bool // set during Close to suppress further writes

	// Current absolute output position. We track this ourselves so
	// the streaming-data-block phase doesn't need to Seek for
	// every offset query.
	outPos uint64

	// entries indexed by canonical absolute path. Insertion order
	// is preserved in entryOrder so behaviour is deterministic.
	entries    map[string]*entry
	entryOrder []string

	// curFile points at the entry whose data is currently being
	// streamed via the io.Writer returned from CreateFile. Replaced
	// by each subsequent Create* call (the prior writer becomes
	// stale).
	curFile *fileWriter

	ids *idTable
}

// NewWriter constructs a Writer that emits its image to out and
// applies the supplied options. WithCompressor is mandatory.
//
// The Writer writes a 96-byte placeholder at file offset 0 so
// callers cannot mistake an unclosed image for a valid one — a
// kernel opening the file before Close would fail the magic check.
func NewWriter(out io.WriteSeeker, opts ...Option) (*Writer, error) {
	cfg := writerConfig{blockSize: DefaultBlockSize}
	for _, opt := range opts {
		if err := opt(&cfg); err != nil {
			return nil, err
		}
	}
	if cfg.compressor == nil {
		return nil, errors.New("squashfs: WithCompressor is required")
	}

	if _, err := out.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("squashfs: seek to start: %w", err)
	}
	placeholder := make([]byte, superblockSize)
	if _, err := out.Write(placeholder); err != nil {
		return nil, fmt.Errorf("squashfs: write placeholder superblock: %w", err)
	}

	return &Writer{
		out:     out,
		cfg:     cfg,
		outPos:  superblockSize,
		entries: map[string]*entry{},
		ids:     newIDTable(),
	}, nil
}

// CreateFile starts a new regular-file entry and returns an
// io.Writer for its contents.
func (w *Writer) CreateFile(attrs Attrs) (io.Writer, error) {
	if w.closed {
		return nil, ErrClosed
	}
	if err := w.flushCurFile(); err != nil {
		return nil, err
	}
	if err := validatePath(attrs.Path); err != nil {
		return nil, err
	}
	if _, exists := w.entries[attrs.Path]; exists {
		return nil, fmt.Errorf("squashfs: duplicate entry %q", attrs.Path)
	}
	e := &entry{
		path:            attrs.Path,
		kind:            kindFile,
		attrs:           attrs,
		fileBlocksStart: w.outPos,
	}
	w.entries[attrs.Path] = e
	w.entryOrder = append(w.entryOrder, attrs.Path)
	w.curFile = &fileWriter{w: w, e: e}
	return w.curFile, nil
}

// CreateDir creates a directory entry.
func (w *Writer) CreateDir(attrs Attrs) error {
	if err := w.beginEntry(attrs.Path); err != nil {
		return err
	}
	w.entries[attrs.Path] = &entry{path: attrs.Path, kind: kindDir, attrs: attrs}
	w.entryOrder = append(w.entryOrder, attrs.Path)
	return nil
}

// CreateSymlink creates a symbolic-link entry.
func (w *Writer) CreateSymlink(attrs Attrs, target string) error {
	if err := w.beginEntry(attrs.Path); err != nil {
		return err
	}
	if target == "" {
		return errors.New("squashfs: symlink target must be non-empty")
	}
	w.entries[attrs.Path] = &entry{
		path:          attrs.Path,
		kind:          kindSymlink,
		attrs:         attrs,
		symlinkTarget: target,
	}
	w.entryOrder = append(w.entryOrder, attrs.Path)
	return nil
}

// CreateHardlink adds an additional directory entry at path
// referencing the inode of target. target must already exist and be
// a regular file.
func (w *Writer) CreateHardlink(linkPath, target string) error {
	if w.closed {
		return ErrClosed
	}
	if err := w.flushCurFile(); err != nil {
		return err
	}
	if err := validatePath(linkPath); err != nil {
		return err
	}
	if err := validatePath(target); err != nil {
		return err
	}
	if _, exists := w.entries[linkPath]; exists {
		return fmt.Errorf("squashfs: duplicate entry %q", linkPath)
	}
	tgt, ok := w.entries[target]
	if !ok || tgt.kind != kindFile {
		return ErrHardlinkTarget
	}
	w.entries[linkPath] = &entry{
		path:           linkPath,
		kind:           kindHardlink,
		hardlinkTarget: tgt,
	}
	w.entryOrder = append(w.entryOrder, linkPath)
	return nil
}

// CreateCharDevice creates a character-device node.
func (w *Writer) CreateCharDevice(attrs Attrs, major, minor uint32) error {
	if err := w.beginEntry(attrs.Path); err != nil {
		return err
	}
	w.entries[attrs.Path] = &entry{
		path: attrs.Path, kind: kindCharDev, attrs: attrs,
		devMajor: major, devMinor: minor,
	}
	w.entryOrder = append(w.entryOrder, attrs.Path)
	return nil
}

// CreateBlockDevice creates a block-device node.
func (w *Writer) CreateBlockDevice(attrs Attrs, major, minor uint32) error {
	if err := w.beginEntry(attrs.Path); err != nil {
		return err
	}
	w.entries[attrs.Path] = &entry{
		path: attrs.Path, kind: kindBlockDev, attrs: attrs,
		devMajor: major, devMinor: minor,
	}
	w.entryOrder = append(w.entryOrder, attrs.Path)
	return nil
}

// CreateFIFO creates a named-pipe entry.
func (w *Writer) CreateFIFO(attrs Attrs) error {
	if err := w.beginEntry(attrs.Path); err != nil {
		return err
	}
	w.entries[attrs.Path] = &entry{path: attrs.Path, kind: kindFIFO, attrs: attrs}
	w.entryOrder = append(w.entryOrder, attrs.Path)
	return nil
}

// CreateSocket creates a unix-socket entry.
func (w *Writer) CreateSocket(attrs Attrs) error {
	if err := w.beginEntry(attrs.Path); err != nil {
		return err
	}
	w.entries[attrs.Path] = &entry{path: attrs.Path, kind: kindSocket, attrs: attrs}
	w.entryOrder = append(w.entryOrder, attrs.Path)
	return nil
}

// beginEntry centralizes the bookkeeping every Create* (except
// CreateFile, which needs a writer to return, and CreateHardlink,
// which has its own validation) shares: ensure-not-closed, flush
// in-flight file, validate path, reject duplicates.
func (w *Writer) beginEntry(p string) error {
	if w.closed {
		return ErrClosed
	}
	if err := w.flushCurFile(); err != nil {
		return err
	}
	if err := validatePath(p); err != nil {
		return err
	}
	if _, exists := w.entries[p]; exists {
		return fmt.Errorf("squashfs: duplicate entry %q", p)
	}
	return nil
}

// flushCurFile seals any in-progress file's tail data block,
// records its final block-sizes list, and clears curFile. Idempotent.
func (w *Writer) flushCurFile() error {
	if w.curFile == nil {
		return nil
	}
	if err := w.curFile.flushTail(); err != nil {
		return err
	}
	w.curFile.stale = true
	w.curFile = nil
	return nil
}

// validatePath enforces "/path/to/x": leading /, no trailing /, no
// empty / "." / ".." components.
func validatePath(p string) error {
	if p == "" || p == "/" {
		return ErrInvalidPath
	}
	if !strings.HasPrefix(p, "/") {
		return ErrInvalidPath
	}
	if strings.HasSuffix(p, "/") {
		return ErrInvalidPath
	}
	for _, part := range strings.Split(p[1:], "/") {
		if part == "" || part == "." || part == ".." {
			return ErrInvalidPath
		}
	}
	return nil
}

// fileWriter is the io.Writer returned from CreateFile. It buffers
// up to blockSize bytes; full blocks are compressed and emitted to
// the parent Writer's output, with their on-disk sizes appended to
// the entry's block-size list. The final partial block is held
// until the entry is sealed by another Create* call or Close.
type fileWriter struct {
	w     *Writer
	e     *entry
	buf   []byte
	stale bool
	// reusable compression target so we don't allocate per block.
	compressBuf []byte
}

func (fw *fileWriter) Write(p []byte) (int, error) {
	if fw.stale {
		return 0, ErrStaleEntry
	}
	if fw.w.closed {
		return 0, ErrClosed
	}
	written := 0
	for len(p) > 0 {
		need := int(fw.w.cfg.blockSize) - len(fw.buf)
		n := len(p)
		if n > need {
			n = need
		}
		fw.buf = append(fw.buf, p[:n]...)
		written += n
		p = p[n:]
		if uint32(len(fw.buf)) == fw.w.cfg.blockSize {
			if err := fw.emitBlock(); err != nil {
				return written, err
			}
		}
	}
	fw.e.fileSize += uint64(written)
	return written, nil
}

// emitBlock compresses fw.buf (with raw-fallback on
// ErrIncompressible) and writes the result to fw.w.out, appending
// the encoded size to the entry's block-size list. Resets buf.
func (fw *fileWriter) emitBlock() error {
	if len(fw.buf) == 0 {
		return nil
	}
	fw.compressBuf = fw.compressBuf[:0]
	out, err := fw.w.cfg.compressor.Compress(fw.compressBuf, fw.buf)
	var disk []byte
	var sizeWord uint32
	if errors.Is(err, ErrIncompressible) || (err == nil && len(out) >= len(fw.buf)) {
		// Store raw — record uncompressed-bit in the size word.
		disk = fw.buf
		sizeWord = uint32(len(fw.buf)) | DataBlockUncompressedBit
	} else if err != nil {
		return fmt.Errorf("squashfs: compress data block: %w", err)
	} else {
		disk = out
		sizeWord = uint32(len(out))
	}
	n, werr := fw.w.out.Write(disk)
	fw.w.outPos += uint64(n)
	if werr != nil {
		return fmt.Errorf("squashfs: write data block: %w", werr)
	}
	fw.e.fileBlockSizes = append(fw.e.fileBlockSizes, sizeWord)
	fw.compressBuf = out[:0]
	fw.buf = fw.buf[:0]
	return nil
}

// flushTail emits the final partial block (if any). Called by the
// parent Writer when the file's entry is being sealed.
func (fw *fileWriter) flushTail() error {
	if len(fw.buf) == 0 {
		return nil
	}
	return fw.emitBlock()
}

// Close flushes the inode table, directory table, ID table, and
// the final superblock. Required.
func (w *Writer) Close() error {
	if w.closed {
		return ErrClosed
	}
	if err := w.flushCurFile(); err != nil {
		return err
	}
	w.closed = true

	root, err := w.buildTree()
	if err != nil {
		return err
	}
	w.assignInodeNumbers(root)
	w.computeLinkCounts(root)
	if err := w.internIDs(root); err != nil {
		return err
	}

	inodeMeta := newMetaWriter(w.cfg.compressor)
	dirMeta := newMetaWriter(w.cfg.compressor)

	// Pass 1: write every non-dir, non-hardlink inode. Their
	// positions in the inode table become known here so that
	// directory listings (built in pass 2) can reference them.
	if err := w.writeLeafInodes(root, inodeMeta); err != nil {
		return err
	}

	// Resolve hardlink inodeRefs to their targets' (now-known)
	// positions. A hardlink doesn't get its own inode in the
	// inode table; its directory entry simply quotes the target's
	// inode coordinates.
	for _, p := range w.entryOrder {
		e := w.entries[p]
		if e.kind == kindHardlink {
			e.inodeRef = e.hardlinkTarget.inodeRef
			e.inodeNumber = e.hardlinkTarget.inodeNumber
		}
	}

	// Pass 2: post-order over directories. For each dir, build
	// its listing in the dir table (using children's known inode
	// positions), then write the dir's own inode.
	if err := w.writeDirs(root, inodeMeta, dirMeta); err != nil {
		return err
	}

	if err := inodeMeta.Flush(); err != nil {
		return err
	}
	if err := dirMeta.Flush(); err != nil {
		return err
	}

	// Emit everything past the data-block region.
	sb := superblock{
		inodeCount:         w.totalInodeCount(),
		modTime:            w.imageMtime(),
		blockSize:          w.cfg.blockSize,
		fragCount:          0,
		compressor:         uint16(w.cfg.compressor.ID()),
		blockLog:           blockLogFor(w.cfg.blockSize),
		flags:              FlagNoFragments | FlagNoXattrs,
		idCount:            w.ids.Count(),
		xattrTableStart:    0xFFFFFFFFFFFFFFFF,
		fragmentTableStart: 0xFFFFFFFFFFFFFFFF,
		exportTableStart:   0xFFFFFFFFFFFFFFFF,
	}

	sb.inodeTableStart = w.outPos
	n, err := inodeMeta.EmitTo(w.out)
	if err != nil {
		return fmt.Errorf("squashfs: emit inode table: %w", err)
	}
	w.outPos += n

	sb.directoryTableStart = w.outPos
	n, err = dirMeta.EmitTo(w.out)
	if err != nil {
		return fmt.Errorf("squashfs: emit directory table: %w", err)
	}
	w.outPos += n

	// ID table: data blocks first (compressed metadata blocks),
	// then the lookup list. The superblock points at the lookup
	// list, not the data.
	idDataStart := w.outPos
	blockOffsets, idBytes, err := w.ids.WriteData(w.out, w.cfg.compressor, idDataStart)
	if err != nil {
		return fmt.Errorf("squashfs: emit ID table data: %w", err)
	}
	w.outPos += idBytes

	sb.idTableStart = w.outPos
	lookupBytes, err := w.ids.WriteLookup(w.out, blockOffsets)
	if err != nil {
		return fmt.Errorf("squashfs: emit ID lookup: %w", err)
	}
	w.outPos += lookupBytes

	sb.bytesUsed = w.outPos

	// Pad the output file to a 4 KiB boundary. bytes_used stays at
	// the unpadded value — mksquashfs does the same, and the
	// kernel's squashfs driver uses bytes_used only for table-range
	// validation, not file-size. The padding exists because the
	// kernel reads via sb_bread at the FS's logical block size
	// (squashfs sets this to 1024 by default); if the underlying
	// block device's reported size doesn't cover the last logical
	// block containing meaningful data, sb_bread short-reads and
	// fill_super returns -EIO silently (no SQUASHFS error: log
	// line — the driver bails before its error-logging path).
	// Symptom we hit: kernel mount panic with no driver-level
	// diagnostics. Aligning to 4 KiB (matching mksquashfs's default)
	// guarantees the block-device read window always covers
	// bytes_used regardless of the host's reported sector size.
	if rem := w.outPos % padAlignment; rem != 0 {
		pad := make([]byte, padAlignment-rem)
		n, err := w.out.Write(pad)
		if err != nil {
			return fmt.Errorf("squashfs: pad to %d-byte boundary: %w", padAlignment, err)
		}
		w.outPos += uint64(n)
	}

	// Root inode reference: top 48 bits = block byte offset in
	// inode table; bottom 16 bits = in-block offset.
	rootBlockByte := inodeMeta.BlockByteOffset(root.inodeRef.blockIdx)
	sb.rootInodeRef = root.inodeRef.encode(rootBlockByte)

	if _, err := w.out.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("squashfs: seek for final superblock: %w", err)
	}
	if _, err := w.out.Write(sb.encode()); err != nil {
		return fmt.Errorf("squashfs: write final superblock: %w", err)
	}
	w.finished = true
	return nil
}

// padAlignment is the 4 KiB boundary every produced image is padded
// out to. See the inline comment in Close for why.
const padAlignment uint64 = 4096

// buildTree wires every entry into a parent → children tree,
// synthesizing the root directory if the caller did not create one
// explicitly. Returns an error if any entry's parent path was never
// created.
func (w *Writer) buildTree() (*entry, error) {
	root, ok := w.entries["/"]
	if !ok {
		root = &entry{
			path:  "/",
			kind:  kindDir,
			attrs: Attrs{Mode: 0o755, Mtime: w.cfg.mtime},
		}
		// The synthetic root is NOT added to entryOrder — it
		// has no path that callers will use to look it up.
		w.entries["/"] = root
	}
	for _, p := range w.entryOrder {
		if p == "/" {
			continue
		}
		e := w.entries[p]
		parentPath := path.Dir(p)
		parent, ok := w.entries[parentPath]
		if !ok {
			return nil, fmt.Errorf("squashfs: entry %q has no parent directory %q", p, parentPath)
		}
		if parent.kind != kindDir {
			return nil, fmt.Errorf("squashfs: parent %q of %q is not a directory", parentPath, p)
		}
		parent.children = append(parent.children, e)
	}
	// Sort children for determinism; the squashfs format also
	// requires sorted directory listings.
	var sortRec func(*entry)
	sortRec = func(d *entry) {
		sort.Slice(d.children, func(i, j int) bool {
			return path.Base(d.children[i].path) < path.Base(d.children[j].path)
		})
		for _, c := range d.children {
			if c.kind == kindDir {
				sortRec(c)
			}
		}
	}
	sortRec(root)
	return root, nil
}

// assignInodeNumbers walks the tree in DFS preorder and assigns
// each entry that owns a real inode a sequential number starting
// at 1 for root. Hardlinks share their target's number — they do
// not consume an inode number.
func (w *Writer) assignInodeNumbers(root *entry) {
	var next uint32 = 1
	var walk func(*entry, uint32)
	walk = func(e *entry, parentInode uint32) {
		if e.kind == kindHardlink {
			return
		}
		e.inodeNumber = next
		e.parentInode = parentInode
		next++
		if e.kind == kindDir {
			for _, c := range e.children {
				walk(c, e.inodeNumber)
			}
		}
	}
	walk(root, 1) // root's parent is itself (inode 1)
}

// computeLinkCounts fills nlinks on every entry. Directories follow
// the Unix convention (2 + number of subdirectories) so callers
// who stat() inside the mounted image see expected values. Files
// start at 1 and gain one per hardlink referencing them. All other
// kinds get 1.
func (w *Writer) computeLinkCounts(root *entry) {
	// Reset/initialize.
	for _, e := range w.entries {
		switch e.kind {
		case kindFile, kindSymlink, kindCharDev, kindBlockDev, kindFIFO, kindSocket:
			e.nlinks = 1
		case kindHardlink:
			if e.hardlinkTarget != nil {
				e.hardlinkTarget.nlinks++
			}
		}
	}
	// Directory link counts: 2 + subdir count.
	var walk func(*entry)
	walk = func(d *entry) {
		var subdirs uint32
		for _, c := range d.children {
			if c.kind == kindDir {
				subdirs++
				walk(c)
			}
		}
		d.nlinks = 2 + subdirs
	}
	walk(root)
}

// internIDs assigns each entry's uidIdx/gidIdx by interning its
// numeric IDs in w.ids. Hardlinks inherit their target's indices.
func (w *Writer) internIDs(root *entry) error {
	for _, p := range w.entryOrder {
		e := w.entries[p]
		if e.kind == kindHardlink {
			continue
		}
		u, err := w.ids.Intern(e.attrs.UID)
		if err != nil {
			return err
		}
		g, err := w.ids.Intern(e.attrs.GID)
		if err != nil {
			return err
		}
		e.uidIdx, e.gidIdx = u, g
	}
	// Root (if synthetic) is not in entryOrder.
	if _, synthetic := w.entries["/"]; synthetic && root.uidIdx == 0 && root.gidIdx == 0 {
		u, err := w.ids.Intern(root.attrs.UID)
		if err != nil {
			return err
		}
		g, err := w.ids.Intern(root.attrs.GID)
		if err != nil {
			return err
		}
		root.uidIdx, root.gidIdx = u, g
	}
	return nil
}

// writeLeafInodes writes every non-dir, non-hardlink entry's inode
// into inodeMeta and records its inodeRef. Order is insertion order
// (entryOrder) so layout is reproducible across runs.
func (w *Writer) writeLeafInodes(root *entry, inodeMeta *metaWriter) error {
	for _, p := range w.entryOrder {
		e := w.entries[p]
		if e.kind == kindDir || e.kind == kindHardlink {
			continue
		}
		if err := w.writeOneInode(e, inodeMeta); err != nil {
			return err
		}
	}
	return nil
}

// writeDirs walks the tree post-order; for each directory it writes
// the listing (children's dir entries, batched under directory
// headers) into dirMeta, then writes the directory's own inode into
// inodeMeta.
func (w *Writer) writeDirs(root *entry, inodeMeta, dirMeta *metaWriter) error {
	var walk func(*entry) error
	walk = func(d *entry) error {
		// Recurse into subdirs first so their inodes are written
		// before this dir's listing references them.
		for _, c := range d.children {
			if c.kind == kindDir {
				if err := walk(c); err != nil {
					return err
				}
			}
		}
		// Build dir entries from now-resolved children.
		entries := make([]dirEntry, 0, len(d.children))
		for _, c := range d.children {
			blockByte := uint64(0)
			// All children's inodeRefs are set: non-dirs from
			// writeLeafInodes, dirs from the recursion above,
			// hardlinks copied from their targets after pass 1.
			if c.kind == kindHardlink {
				blockByte = w.inodeBlockByte(c.hardlinkTarget.inodeRef, inodeMeta)
			} else {
				blockByte = w.inodeBlockByte(c.inodeRef, inodeMeta)
			}
			if blockByte > 0xFFFFFFFF {
				return errors.New("squashfs: inode table exceeds 4 GiB")
			}
			entries = append(entries, dirEntry{
				name:           path.Base(c.path),
				childInode:     c.inodeNumber,
				inodeBlockByte: uint32(blockByte),
				inodeBlockOff:  c.inodeRef.offset,
				basicType:      basicTypeFor(c),
			})
		}
		startRef, listingBytes, err := writeDirListing(dirMeta, entries)
		if err != nil {
			return err
		}
		d.dirBlockOffset = startRef.offset
		d.dirBlockStart = dirMeta.BlockByteOffset(startRef.blockIdx)
		d.dirListingSize = listingBytes
		return w.writeOneInode(d, inodeMeta)
	}
	return walk(root)
}

// inodeBlockByte returns the block-byte component of an inodeRef,
// given the inodeMeta it lives in. Helper to keep call sites tidy.
func (w *Writer) inodeBlockByte(r metaRef, inodeMeta *metaWriter) uint64 {
	return inodeMeta.BlockByteOffset(r.blockIdx)
}

// writeOneInode dispatches to the type-specific encoder for e and
// appends the bytes to inodeMeta, recording e.inodeRef.
func (w *Writer) writeOneInode(e *entry, inodeMeta *metaWriter) error {
	mtime, err := w.entryMtime(e)
	if err != nil {
		return fmt.Errorf("squashfs: %s: %w", e.path, err)
	}
	var body []byte
	switch e.kind {
	case kindFile:
		body, err = encodeBasicFile(e, mtime, w.cfg.blockSize)
	case kindDir:
		body, err = encodeBasicDir(e, mtime)
	case kindSymlink:
		body, err = encodeBasicSymlink(e, mtime)
	case kindCharDev:
		body, err = encodeBasicDevice(e, mtime, false)
	case kindBlockDev:
		body, err = encodeBasicDevice(e, mtime, true)
	case kindFIFO:
		body = encodeBasicIPC(e, mtime, false)
	case kindSocket:
		body = encodeBasicIPC(e, mtime, true)
	default:
		return fmt.Errorf("squashfs: unsupported entry kind for %q", e.path)
	}
	if err != nil {
		return fmt.Errorf("squashfs: encode inode for %q: %w", e.path, err)
	}
	e.inodeRef = inodeMeta.Ref()
	if _, err := inodeMeta.Write(body); err != nil {
		return err
	}
	return nil
}

// entryMtime resolves the seconds-since-epoch mtime for an entry,
// honoring WithFixedMtime if set. A zero-value time.Time is
// interpreted as Unix epoch 0 (the natural reading of "unset"),
// not the Go zero year which would overflow the uint32 field.
func (w *Writer) entryMtime(e *entry) (uint32, error) {
	t := e.attrs.Mtime
	if !w.cfg.mtime.IsZero() {
		t = w.cfg.mtime
	}
	if t.IsZero() {
		return 0, nil
	}
	sec := t.Unix()
	if sec < 0 || sec > 0xFFFFFFFF {
		return 0, errors.New("squashfs: mtime out of uint32 epoch range")
	}
	return uint32(sec), nil
}

// totalInodeCount returns the number of distinct inodes (excludes
// hardlinks, which share inodes).
func (w *Writer) totalInodeCount() uint32 {
	var n uint32
	for _, e := range w.entries {
		if e.kind != kindHardlink {
			n++
		}
	}
	return n
}

// imageMtime is the seconds-since-epoch value stamped into the
// superblock's mod_time field. Uses the fixed-mtime override if
// set; otherwise picks the max mtime over all entries (clamped to
// the uint32 range).
func (w *Writer) imageMtime() uint32 {
	if !w.cfg.mtime.IsZero() {
		sec := w.cfg.mtime.Unix()
		if sec < 0 {
			return 0
		}
		if sec > 0xFFFFFFFF {
			return 0xFFFFFFFF
		}
		return uint32(sec)
	}
	var max int64
	for _, e := range w.entries {
		if e.attrs.Mtime.IsZero() {
			continue
		}
		s := e.attrs.Mtime.Unix()
		if s > max {
			max = s
		}
	}
	if max < 0 {
		return 0
	}
	if max > 0xFFFFFFFF {
		return 0xFFFFFFFF
	}
	return uint32(max)
}

// Compile-time guard so we get an unused-import warning if encoding
// drift removes the only encoding/binary user from this file.
var _ = binary.LittleEndian
