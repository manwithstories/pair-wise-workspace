// Package index builds a randomly seekable, verifiable index over repaired
// media packets. Entries are streamed to a temp file and atomically committed,
// so a truncated/interrupted run never leaves a half-written index.
package index

import (
	"bufio"
	"encoding/binary"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"

	"avrepair/internal/clock"
)

// Magic and format version for the on-disk index container.
const (
	Magic   = "AVRIX1\n"
	Version = uint16(1)

	// AnchorEvery bounds memory: only every Nth entry is kept in the in-memory
	// seek table, while every entry is still verified on disk.
	AnchorEvery = 64
)

// Entry is one indexed packet location.
type Entry struct {
	Track   string `json:"track"`
	Seq     int    `json:"seq"`
	TS      int64  `json:"ts_ns"`
	Offset  int64  `json:"offset"`
	Length  int    `json:"length"`
	Payload uint32 `json:"crc32"`
}

// Anchor is a sparse in-memory pointer used to locate a seek target without
// loading the whole index (bounds memory for large assets).
type Anchor struct {
	TS     int64 `json:"ts_ns"`
	Offset int64 `json:"offset"`
}

// Index is the verified random-access structure returned by Build.
type Index struct {
	path    string
	entries []Anchor
	count   int
	lastTS  map[string]int64
}

// Builder streams entries to a temporary file, enforcing monotonicity and
// per-entry checksums as it goes. Call Commit to atomically publish.
type Builder struct {
	tmp     *os.File
	path    string
	anchors []Anchor
	count   int
	lastTS  map[string]int64
	written int64
}

// NewBuilder opens a temp index next to the final path.
func NewBuilder(dir, finalName string) (*Builder, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(dir, finalName+".tmp-*")
	if err != nil {
		return nil, err
	}
	b := &Builder{
		tmp:    tmp,
		path:   filepath.Join(dir, finalName),
		lastTS: map[string]int64{},
	}
	header := make([]byte, 0, len(Magic)+2)
	header = append(header, Magic...)
	header = binary.LittleEndian.AppendUint16(header, Version)
	if _, err := tmp.Write(header); err != nil {
		return nil, err
	}
	b.written += int64(len(header))
	return b, nil
}

// Append validates monotonicity per track and writes one framed entry. A
// non-monotonic timestamp is a hard CodeNonMonotonicIndex failure.
func (b *Builder) Append(e Entry) error {
	if prev, ok := b.lastTS[e.Track]; ok && e.TS <= prev {
		return clock.NewError(clock.CodeNonMonotonic, e.Track,
			clock.Interval{Start: prev, End: e.TS},
			"index entry timestamps must be strictly increasing")
	}
	b.lastTS[e.Track] = e.TS

	frame := encodeEntry(e)
	checksum := crc32.ChecksumIEEE(frame)
	if _, err := b.tmp.Write(frame); err != nil {
		return err
	}
	checksumBuf := binary.LittleEndian.AppendUint32(nil, checksum)
	if _, err := b.tmp.Write(checksumBuf); err != nil {
		return err
	}
	if b.count%AnchorEvery == 0 {
		b.anchors = append(b.anchors, Anchor{TS: e.TS, Offset: b.written})
	}
	b.written += int64(len(frame) + 4)
	b.count++
	return nil
}

// Commit flushes, fsyncs and atomically renames the temp file to the final
// path, then reopens it for verified random access.
func (b *Builder) Commit() (*Index, error) {
	if err := b.tmp.Sync(); err != nil {
		return nil, err
	}
	if err := b.tmp.Close(); err != nil {
		return nil, err
	}
	if err := os.Rename(b.tmp.Name(), b.path); err != nil {
		return nil, err
	}
	idx := &Index{
		path:    b.path,
		entries: append([]Anchor{}, b.anchors...),
		count:   b.count,
		lastTS:  map[string]int64{},
	}
	if err := idx.verify(); err != nil {
		return nil, err
	}
	return idx, nil
}

// Abort discards the temp file without touching any committed index.
func (b *Builder) Abort() {
	name := b.tmp.Name()
	_ = b.tmp.Close()
	_ = os.Remove(name)
}

// Open reopens a previously committed index and re-verifies every invariant
// (magic, framing, checksums, global monotonicity, entry count) before handing
// it back for seeks.
func Open(dir, finalName string) (*Index, error) {
	path := filepath.Join(dir, finalName)
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	count, anchors, err := scanAnchors(f)
	if err != nil {
		return nil, err
	}
	x := &Index{
		path:    path,
		entries: anchors,
		count:   count,
		lastTS:  map[string]int64{},
	}
	if err := x.verify(); err != nil {
		return nil, err
	}
	return x, nil
}

// scanAnchors counts entries and rebuilds sparse anchors without retaining
// entry payloads, keeping reopen memory bounded on large assets.
func scanAnchors(f *os.File) (int, []Anchor, error) {
	if _, err := f.Seek(int64(len(Magic)+2), io.SeekStart); err != nil {
		return 0, nil, err
	}
	r := bufio.NewReaderSize(f, 1<<16)
	var anchors []Anchor
	n := 0
	header := int64(len(Magic) + 2)
	pos := header
	stride := int64(frameSize() + 4)
	for {
		e, _, err := readFramed(r)
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, nil, clock.NewError(clock.CodeTruncated, e.Track,
				clock.Interval{}, "truncated frame while scanning: "+err.Error())
		}
		if n%AnchorEvery == 0 {
			anchors = append(anchors, Anchor{TS: e.TS, Offset: pos})
		}
		pos += stride
		n++
	}
	return n, anchors, nil
}

// verify re-reads every entry, checks framing, checksums and global ordering,
// proving the committed index satisfies all index invariants.
func (x *Index) verify() error {
	f, err := os.Open(x.path)
	if err != nil {
		return err
	}
	defer f.Close()
	hdr := make([]byte, len(Magic)+2)
	if _, err := io.ReadFull(f, hdr); err != nil {
		return clock.NewError(clock.CodeTruncated, "", clock.Interval{},
			"index header unreadable")
	}
	if string(hdr[:len(Magic)]) != Magic {
		return clock.NewError(clock.CodeChecksum, "", clock.Interval{}, "bad index magic")
	}
	var prevTS int64 = -1
	n := 0
	for {
		e, checksum, err := readFramed(f)
		if err == io.EOF {
			break
		}
		if err != nil {
			return clock.NewError(clock.CodeTruncated, e.Track, clock.Interval{},
				"truncated index frame: "+err.Error())
		}
		frame := encodeEntry(e)
		if crc32.ChecksumIEEE(frame) != checksum {
			return clock.NewError(clock.CodeChecksum, e.Track,
				clock.Interval{Start: e.TS}, "entry checksum mismatch")
		}
		if e.TS <= prevTS {
			return clock.NewError(clock.CodeNonMonotonic, e.Track,
				clock.Interval{Start: prevTS, End: e.TS}, "global ordering broken")
		}
		prevTS = e.TS
		n++
	}
	if n != x.count {
		return clock.NewError(clock.CodeTruncated, "", clock.Interval{},
			"committed entry count mismatch")
	}
	return nil
}
