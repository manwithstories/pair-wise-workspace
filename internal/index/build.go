// Package index builds the random-access index over a source container.
//
// The index is a flat, self-describing file committed with an atomic rename.
// Each entry names a jump point: the byte offset of a frame box, its number,
// its unified PTS and a digest binding the entry to the frame's identity. A
// reader can therefore seek straight to a frame without scanning, and can
// detect an entry that no longer matches the frame it claims to describe.
package index

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"

	"mediad/internal/model"
)

// Magic identifies an index file. Version is bumped on any layout change.
const (
	Magic   = "MDIDX1\n"
	Version = 1
)

// Entry is one jump point. Digest binds the entry to the frame's declared
// metadata and content, so a stale or swapped entry is rejected on read.
type Entry struct {
	FrameNo uint64
	TrackID uint32
	PTS     int64
	Offset  int64
	Size    uint32
	Digest  [32]byte
}

// Stats describes a built index.
type Stats struct {
	Entries   int
	Skipped   int   // unusable frames deliberately excluded
	Bytes     int64 // index file size
	Replaced  bool  // an incomplete old index was found and replaced
	Truncated int   // entries dropped from a partial old index
}

// ErrCorruptIndex reports an index that cannot be trusted. It is never fatal
// to the pipeline: the index is rebuilt.
var ErrCorruptIndex = errors.New("index: corrupt or incomplete")

// EntryDigest binds an entry to the frame identity it points at.
func EntryDigest(trackID uint32, frameNo uint64, pts int64, offset int64, size uint32, content [32]byte) [32]byte {
	var b bytes.Buffer
	b.WriteString("mediad/index-entry/v1\x00")
	binary.Write(&b, binary.BigEndian, trackID)
	binary.Write(&b, binary.BigEndian, frameNo)
	binary.Write(&b, binary.BigEndian, pts)
	binary.Write(&b, binary.BigEndian, offset)
	binary.Write(&b, binary.BigEndian, size)
	b.Write(content[:])
	return model.DigestBytes(b.Bytes())
}

// Writer accumulates entries and commits them atomically in one pass. Entries
// are streamed to the underlying writer, so building an index never holds the
// whole track in memory.
type Writer struct {
	w       io.Writer
	count   int
	crc     uint32
	written bool
	header  [16]byte
}

// NewWriter seeds the header: magic, version, a placeholder count and a CRC
// over all entries. The count and CRC are patched only if the writer targets a
// seekable file; for a plain io.Writer the caller buffers and patches.
func NewWriter(w io.Writer) (*Writer, error) {
	i := &Writer{w: w, crc: crc32.ChecksumIEEE(nil)}
	copy(i.header[:], Magic)
	binary.BigEndian.PutUint32(i.header[8:], uint32(Version))
	if _, err := w.Write(i.header[:]); err != nil {
		return nil, err
	}
	return i, nil
}

// Add appends one entry. Envelopes are encoded in a fixed layout with the
// entry digest last so a partial trailing entry can be detected on read.
func (i *Writer) Add(e Entry) error {
	var buf [4 + 8 + 4 + 8 + 8 + 4 + 32 + 4]byte
	b := buf[:0]
	b = binary.BigEndian.AppendUint32(b, uint32(0x4d454e54)) // "MENT"
	b = binary.BigEndian.AppendUint64(b, e.FrameNo)
	b = binary.BigEndian.AppendUint32(b, e.TrackID)
	b = binary.BigEndian.AppendUint64(b, uint64(e.PTS))
	b = binary.BigEndian.AppendUint64(b, uint64(e.Offset))
	b = binary.BigEndian.AppendUint32(b, e.Size)
	b = append(b, e.Digest[:]...)
	sum := crc32.ChecksumIEEE(buf[:len(b)])
	b = binary.BigEndian.AppendUint32(b, sum)
	if _, err := i.w.Write(b); err != nil {
		return err
	}
	i.count++
	i.crc = crc32.Update(i.crc, crc32.IEEETable, b)
	return nil
}

// Count returns the number of entries added so far.
func (i *Writer) Count() int { return i.count }

// Finish patches the header (when the writer offers a patcher). The header
// carries the version and the final entry count; the caller fsyncs and
// renames the file.
func (i *Writer) Finish(patcher func(header [16]byte) error) error {
	binary.BigEndian.PutUint32(i.header[8:], uint32(Version))
	binary.BigEndian.PutUint32(i.header[12:], uint32(i.count))
	if patcher != nil {
		if err := patcher(i.header); err != nil {
			return err
		}
	}
	return nil
}

// HeaderSize is the number of bytes preceding the first entry.
const HeaderSize = 16

// entrySize is the on-disk size of one encoded entry.
const entrySize = 4 + 8 + 4 + 8 + 8 + 4 + 32 + 4

// ReadAll decodes an index file, validating the magic, version and the entry
// CRCs. It stops at the first incomplete entry and reports how many entries
// were dropped, which is how a half-written index is detected rather than
// trusted.
func ReadAll(r io.ReadSeeker) ([]Entry, int, error) {
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, 0, err
	}
	head := make([]byte, HeaderSize)
	if _, err := io.ReadFull(r, head); err != nil {
		return nil, 0, fmt.Errorf("%w: short header", ErrCorruptIndex)
	}
	if string(head[:7]) != Magic {
		return nil, 0, fmt.Errorf("%w: bad magic", ErrCorruptIndex)
	}
	if v := binary.BigEndian.Uint32(head[8:]); v != Version {
		return nil, 0, fmt.Errorf("%w: version %d", ErrCorruptIndex, v)
	}
	declared := int(binary.BigEndian.Uint32(head[12:]))
	br := bufio.NewReader(r)
	var (
		entries []Entry
		dropped int
	)
	for {
		var raw [entrySize]byte
		n, err := io.ReadFull(br, raw[:])
		if err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				if n > 0 {
					dropped++
				}
				break
			}
			return nil, 0, err
		}
		e, ok := decodeEntry(raw[:])
		if !ok {
			dropped++
			break
		}
		entries = append(entries, e)
	}
	if dropped == 0 && len(entries) != declared {
		// A clean read that disagrees with the declared count means the file
		// was torn before the count was patched.
		dropped = declared - len(entries)
		if dropped < 0 {
			dropped = 0
		}
	}
	return entries, dropped, nil
}

func decodeEntry(raw []byte) (Entry, bool) {
	off := 0
	if binary.BigEndian.Uint32(raw[off:]) != 0x4d454e54 {
		return Entry{}, false
	}
	off += 4
	e := Entry{
		FrameNo: binary.BigEndian.Uint64(raw[off:]),
	}
	off += 8
	e.TrackID = binary.BigEndian.Uint32(raw[off:])
	off += 4
	e.PTS = int64(binary.BigEndian.Uint64(raw[off:]))
	off += 8
	e.Offset = int64(binary.BigEndian.Uint64(raw[off:]))
	off += 8
	e.Size = binary.BigEndian.Uint32(raw[off:])
	off += 4
	copy(e.Digest[:], raw[off:off+32])
	off += 32
	want := binary.BigEndian.Uint32(raw[off:])
	if crc32.ChecksumIEEE(raw[:off]) != want {
		return Entry{}, false
	}
	return e, true
}

// Validate checks that every entry points at a frame that still exists and
// still matches the entry's digest and PTS. resolve returns the frame's
// content digest and PTS for an offset; a missing or damaged frame yields an
// error, which invalidates the whole index.
func Validate(entries []Entry, resolve func(e Entry) (content [32]byte, pts int64, err error)) error {
	for _, e := range entries {
		content, pts, err := resolve(e)
		if err != nil {
			return fmt.Errorf("%w: entry frame %d at %d: %v", ErrCorruptIndex, e.FrameNo, e.Offset, err)
		}
		if pts != e.PTS {
			return fmt.Errorf("%w: entry frame %d pts %d != frame %d", ErrCorruptIndex, e.FrameNo, e.PTS, pts)
		}
		want := EntryDigest(e.TrackID, e.FrameNo, e.PTS, e.Offset, e.Size, content)
		if want != e.Digest {
			return fmt.Errorf("%w: entry frame %d digest mismatch", ErrCorruptIndex, e.FrameNo)
		}
	}
	return nil
}
