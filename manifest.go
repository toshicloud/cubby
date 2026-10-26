// Copyright 2026 The cubby contributors
// SPDX-License-Identifier: Apache-2.0

package cubby

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

const (
	// manifestName is the file that publishes an entry's block list. Its
	// presence is also what makes an upload's blocks committed: no later
	// write to the upload may change them.
	manifestName = "manifest.json"
	// blocksDirName holds the staged blocks of an upload and, once finalized,
	// of the entry.
	blocksDirName = "blocks"
	// manifestVersion is the only layout written today.
	manifestVersion = 1
)

// errEntryMiss reports an entry whose stored bytes or manifest are invalid.
var errEntryMiss = errors.New("cubby: entry miss")

// entryManifest names the block files that make up one entry, in byte order.
// One block may be listed more than once, and then its bytes appear once per
// place. Offsets come from the sizes here, never from the block IDs.
type entryManifest struct {
	Version  int               `json:"version"`
	Size     int64             `json:"size"`
	Segments []manifestSegment `json:"segments"`
}

// manifestSegment is one block file and the bytes it contributes.
type manifestSegment struct {
	Block string `json:"block"` // hex-encoded block ID, the file name under blocks/
	Size  int64  `json:"size"`
}

// readManifest reads and validates one published manifest.
func readManifest(path string) (entryManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return entryManifest{}, err
	}
	var m entryManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return entryManifest{}, fmt.Errorf("%w: parse manifest: %w", errEntryMiss, err)
	}
	if m.Version != manifestVersion || len(m.Segments) == 0 {
		return entryManifest{}, fmt.Errorf("%w: unsupported manifest %q", errEntryMiss, path)
	}
	var total int64
	for _, seg := range m.Segments {
		if seg.Block == "" || filepath.Base(seg.Block) != seg.Block || seg.Size < 0 {
			return entryManifest{}, fmt.Errorf("%w: bad manifest segment %q", errEntryMiss, seg.Block)
		}
		total += seg.Size
	}
	if total != m.Size {
		return entryManifest{}, fmt.Errorf("%w: manifest size %d, segments %d", errEntryMiss, m.Size, total)
	}
	return m, nil
}

// writeManifest publishes a manifest in dir: a temporary file, fsync, rename,
// then an fsync of the directory. The rename is the single point at which the
// list becomes visible, and a crash before it leaves no manifest at all.
func writeManifest(dir string, m entryManifest) error {
	data, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("cubby: marshal manifest: %w", err)
	}
	tmp := filepath.Join(dir, manifestName+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("cubby: create manifest: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("cubby: write manifest: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("cubby: sync manifest: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("cubby: close manifest: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, manifestName)); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("cubby: publish manifest: %w", err)
	}
	return syncDir(dir)
}

// syncDir makes a rename inside dir durable.
func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return f.Sync()
}

// segmentReader reads an entry as one stream over its block files. One reader
// belongs to one request and holds at most one file open: a full download of a
// ten-gigabyte entry must not need three hundred descriptors.
type segmentReader struct {
	dir    string
	segs   []manifestSegment
	starts []int64
	size   int64

	cur      *os.File
	curIndex int
}

// openSegmentReader checks that every block file is present at the size its
// manifest records and returns a reader over them. A block that is missing, is
// not a regular file, or is not the recorded size is errEntryMiss: the entry
// must not be served. Any other stat failure is returned as itself, so a
// temporary error does not destroy a good entry.
func openSegmentReader(dir string, m entryManifest) (*segmentReader, error) {
	r := &segmentReader{
		dir:      dir,
		segs:     m.Segments,
		starts:   make([]int64, len(m.Segments)),
		size:     m.Size,
		curIndex: -1,
	}
	var off int64
	for i, seg := range m.Segments {
		r.starts[i] = off
		fi, err := os.Stat(filepath.Join(dir, blocksDirName, seg.Block))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return nil, fmt.Errorf("%w: block %s is missing", errEntryMiss, seg.Block)
		case err != nil:
			return nil, fmt.Errorf("cubby: stat block %s: %w", seg.Block, err)
		case !fi.Mode().IsRegular(), fi.Size() != seg.Size:
			return nil, fmt.Errorf("%w: block %s is %d bytes, not %d", errEntryMiss, seg.Block, fi.Size(), seg.Size)
		}
		off += seg.Size
	}
	if off != m.Size {
		return nil, fmt.Errorf("%w: segments total %d, manifest %d", errEntryMiss, off, m.Size)
	}
	return r, nil
}

// ReadAt reads at off from the entry's first byte. A read that starts past the
// end is io.EOF, and so is the last one; a read that spans segments reads each
// file at its own offset.
func (r *segmentReader) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("cubby: negative offset")
	}
	if off >= r.size {
		return 0, io.EOF
	}
	n := 0
	for n < len(p) && off < r.size {
		i := sort.Search(len(r.starts), func(i int) bool { return r.starts[i] > off }) - 1
		if i < 0 {
			return n, fmt.Errorf("cubby: offset %d outside the manifest", off)
		}
		seg := r.segs[i]
		start := off - r.starts[i]
		want := int64(len(p) - n)
		if remain := seg.Size - start; want > remain {
			want = remain
		}
		if remain := r.size - off; want > remain {
			want = remain
		}
		f, err := r.file(i)
		if err != nil {
			return n, err
		}
		read, err := f.ReadAt(p[n:n+int(want)], start)
		n += read
		off += int64(read)
		if err != nil && (!errors.Is(err, io.EOF) || int64(read) != want) {
			return n, err
		}
		if read == 0 {
			return n, io.ErrNoProgress
		}
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// file returns the block file holding segment i, keeping at most that one open
// for the request; a range that jumps back reopens the earlier file.
func (r *segmentReader) file(i int) (*os.File, error) {
	if r.cur != nil && r.curIndex == i {
		return r.cur, nil
	}
	r.closeCurrent()
	f, err := os.Open(filepath.Join(r.dir, blocksDirName, r.segs[i].Block))
	if err != nil {
		return nil, err
	}
	r.cur, r.curIndex = f, i
	return f, nil
}

func (r *segmentReader) closeCurrent() {
	if r.cur == nil {
		return
	}
	_ = r.cur.Close()
	r.cur, r.curIndex = nil, -1
}

// Close releases the block file the reader holds.
func (r *segmentReader) Close() error {
	r.closeCurrent()
	return nil
}
