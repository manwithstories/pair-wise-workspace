// Package adapter implements versioned parsers for the on-disk frame
// container and the MP4 side-car index, plus the deterministic sample
// generator used by tests and the demo command.
package adapter

import (
	"encoding/binary"
	"errors"
	"io"

	"avpipe/internal/media"
)

// Container format version supported by this build.
const (
	ContainerVersion1 uint8 = 1
	// HeaderSize is the fixed byte size of the file header.
	HeaderSize = 28
	// RecordHeaderSize is the fixed byte size of a record header.
	RecordHeaderSize = 20
	// FooterSize is the fixed byte size of the trailing footer.
	FooterSize = 52
	// AudioHeaderSize is the on-disk size of one PCM block descriptor.
	AudioHeaderSize = 32
	// IndexEntrySize is the on-disk size of one index entry.
	IndexEntrySize = 24
	// MaxInputSize bounds any accepted input stream (256 MiB).
	MaxInputSize int64 = 256 << 20
	// MaxRecordSize bounds a single record (16 MiB).
	MaxRecordSize uint32 = 16 << 20
)

// Magic markers.
var (
	MagicHeader = [4]byte{'A', 'V', 'P', 'I'}
	MagicAudio  = [4]byte{'A', 'V', 'P', 'A'}
	MagicFooter = [4]byte{'A', 'V', 'P', 'F'}
)

// Header flags.
const (
	FlagHasIndex uint8 = 1 << 0
	FlagHasAudio uint8 = 1 << 1
)

// ErrShortStream is returned when the input ends before a complete unit.
var ErrShortStream = errors.New("adapter: short stream")

func putU16(b []byte, v uint16) { binary.BigEndian.PutUint16(b, v) }
func putU32(b []byte, v uint32) { binary.BigEndian.PutUint32(b, v) }
func putU64(b []byte, v uint64) { binary.BigEndian.PutUint64(b, v) }
func u16(b []byte) uint16       { return binary.BigEndian.Uint16(b) }
func u32(b []byte) uint32       { return binary.BigEndian.Uint32(b) }
func u64(b []byte) uint64       { return binary.BigEndian.Uint64(b) }

// readFull is a bounds-checked read: it guarantees that len(buf) bytes exist
// inside limit before touching the reader, rejecting overflowing offsets.
// When the reader is an io.ReadSeeker it is first positioned at off, so callers
// may pass absolute offsets regardless of the current cursor.
func readFull(r io.Reader, off int64, buf []byte, limit int64) error {
	if !media.CheckBounds(uint64(off), uint64(len(buf)), uint64(limit)) {
		return media.NewError("parser", media.KindOutOfBounds, off,
			media.ActionFail, "read of %d bytes exceeds limit %d", len(buf), limit)
	}
	if seeker, ok := r.(io.ReadSeeker); ok {
		if _, err := seeker.Seek(off, io.SeekStart); err != nil {
			return media.NewError("parser", "io_error", off, media.ActionFail, "%v", err)
		}
	}
	if _, err := io.ReadFull(r, buf); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return ErrShortStream
		}
		return media.NewError("parser", "io_error", off, media.ActionFail, "%v", err)
	}
	return nil
}

// IndexEntry is one authoritative PTS/offset row in the index section.
type IndexEntry struct {
	Sequence uint32
	PTS90k   uint64
	Offset   uint64
}

// marshalIndexEntry serializes one 24-byte index row:
// seq u32, reserved u32, pts u64, offset u64.
func marshalIndexEntry(e IndexEntry) []byte {
	b := make([]byte, IndexEntrySize)
	putU32(b[0:], e.Sequence)
	putU64(b[8:], e.PTS90k)
	putU64(b[16:], e.Offset)
	return b
}

func parseIndexEntry(b []byte) IndexEntry {
	return IndexEntry{
		Sequence: u32(b[0:]),
		PTS90k:   u64(b[8:]),
		Offset:   u64(b[16:]),
	}
}
