// Package txn provides write-ahead logging and atomic, validated commit
// of aligned media segments.
package txn

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"os"
)

// RecordType identifies a WAL record kind.
type RecordType byte

const (
	RecSegment RecordType = 1 // segment payload, not yet committed
	RecCommit  RecordType = 2 // commit marker for a segment
	RecAbort   RecordType = 3 // explicit abort of a segment
)

const walMagic = 0x4D53574C // "MSWL"

// Record is one WAL entry.
type Record struct {
	Type      RecordType
	SegmentID uint64
	Payload   []byte
}

// WAL is an append-only write-ahead log. Records are CRC-framed so a
// torn tail from an interrupted process is detected and dropped on open.
type WAL struct {
	f *os.File
}

// Open opens (or creates) a WAL, scans it, and truncates any torn or
// corrupt tail. The valid records are returned for replay; torn reports
// whether an uncommitted tail was discarded.
func Open(path string) (*WAL, []Record, bool, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, nil, false, err
	}
	var records []Record
	var good int64
	buf, err := io.ReadAll(f)
	if err != nil {
		f.Close()
		return nil, nil, false, err
	}
	for off := 0; off < len(buf); {
		rec, n, ok := decode(buf[off:])
		if !ok {
			break // torn or corrupt tail: roll back to last good offset
		}
		records = append(records, rec)
		off += n
		good = int64(off)
	}
	torn := good < int64(len(buf))
	if err := f.Truncate(good); err != nil {
		f.Close()
		return nil, nil, false, err
	}
	if _, err := f.Seek(good, io.SeekStart); err != nil {
		f.Close()
		return nil, nil, false, err
	}
	return &WAL{f: f}, records, torn, nil
}

func decode(buf []byte) (Record, int, bool) {
	const hdr = 4 + 1 + 8 + 4
	if len(buf) < hdr+4 {
		return Record{}, 0, false
	}
	if binary.BigEndian.Uint32(buf[0:4]) != walMagic {
		return Record{}, 0, false
	}
	typ := RecordType(buf[4])
	segID := binary.BigEndian.Uint64(buf[5:13])
	n := int(binary.BigEndian.Uint32(buf[13:17]))
	if n < 0 || hdr+n+4 > len(buf) {
		return Record{}, 0, false
	}
	payload := buf[hdr : hdr+n]
	crc := binary.BigEndian.Uint32(buf[hdr+n : hdr+n+4])
	if crc32.ChecksumIEEE(buf[4:hdr+n]) != crc {
		return Record{}, 0, false
	}
	return Record{Type: typ, SegmentID: segID, Payload: append([]byte(nil), payload...)}, hdr + n + 4, true
}

// Append writes one record and flushes it to the OS.
func (w *WAL) Append(rec Record) error {
	n := len(rec.Payload)
	buf := make([]byte, 0, 4+1+8+4+n+4)
	buf = binary.BigEndian.AppendUint32(buf, walMagic)
	buf = append(buf, byte(rec.Type))
	buf = binary.BigEndian.AppendUint64(buf, rec.SegmentID)
	buf = binary.BigEndian.AppendUint32(buf, uint32(n))
	buf = append(buf, rec.Payload...)
	buf = binary.BigEndian.AppendUint32(buf, crc32.ChecksumIEEE(buf[4:]))
	if _, err := w.f.Write(buf); err != nil {
		return fmt.Errorf("txn: wal append: %w", err)
	}
	return w.f.Sync()
}

// Close closes the underlying file.
func (w *WAL) Close() error { return w.f.Close() }
