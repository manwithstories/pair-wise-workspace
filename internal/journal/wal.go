// Package journal provides a write-ahead log with atomic stage commits. Every
// record is length-prefixed, CRC-protected and sequence-numbered so a torn
// tail or a missing record is detectable on restart.
package journal

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
)

// RecordKind classifies WAL entries.
type RecordKind uint8

const (
	// KindBegin marks the start of a stage attempt; it carries the input
	// digest and the input byte range the stage consumed.
	KindBegin RecordKind = 1
	// KindOutput records one output fragment: its offset and hash.
	KindOutput RecordKind = 2
	// KindCommit marks a stage as durably committed with a validation
	// summary.
	KindCommit RecordKind = 3
	// KindRollback closes an uncommitted tail and records how many bytes
	// were discarded.
	KindRollback RecordKind = 4
)

func (k RecordKind) String() string {
	switch k {
	case KindBegin:
		return "begin"
	case KindOutput:
		return "output"
	case KindCommit:
		return "commit"
	case KindRollback:
		return "rollback"
	default:
		return fmt.Sprintf("kind-%d", uint8(k))
	}
}

// Record is one WAL entry. The binary layout is:
//
//	magic(4) | seq(8) | kind(1) | stage(2) | track(4) | offset(8) |
//	length(4) | digest(32) | summaryLen(2) | crc32(4) | summary
//
// The CRC is inside the fixed header so a stream reader can validate a record
// without knowing the summary length in advance. It covers every fixed byte
// before it plus the summary body.
type Record struct {
	Seq     uint64
	Kind    RecordKind
	Stage   uint16
	TrackID int
	Offset  int64
	Digest  [32]byte
	Summary string
}

const recordMagic uint32 = 0x4d4d5741 // "MMWA"

// recordFixedLen is every field including the trailing CRC slot; the
// variable summary body follows it.
const recordFixedLen = 69

// Encode serializes a record.
func Encode(r Record) []byte {
	summary := []byte(r.Summary)
	buf := make([]byte, recordFixedLen+len(summary))
	binary.BigEndian.PutUint32(buf[0:], recordMagic)
	binary.BigEndian.PutUint64(buf[4:], r.Seq)
	buf[12] = byte(r.Kind)
	binary.BigEndian.PutUint16(buf[13:], r.Stage)
	binary.BigEndian.PutUint32(buf[15:], uint32(r.TrackID))
	binary.BigEndian.PutUint64(buf[19:], uint64(r.Offset))
	binary.BigEndian.PutUint32(buf[27:], uint32(len(summary)))
	copy(buf[31:], r.Digest[:])
	binary.BigEndian.PutUint16(buf[63:], uint16(len(summary)))
	copy(buf[recordFixedLen:], summary)
	crc := crc32.ChecksumIEEE(buf[:recordFixedLen-4])
	crc = crc32.Update(crc, crc32.IEEETable, summary)
	binary.BigEndian.PutUint32(buf[recordFixedLen-4:], crc)
	return buf
}

// Decode parses a record and verifies magic and CRC. It returns the number of
// bytes consumed so a stream reader can advance.
func Decode(buf []byte) (Record, int, error) {
	if len(buf) < recordFixedLen {
		return Record{}, 0, io.ErrUnexpectedEOF
	}
	if binary.BigEndian.Uint32(buf) != recordMagic {
		return Record{}, 0, fmt.Errorf("journal: bad record magic")
	}
	summaryLen := int(binary.BigEndian.Uint16(buf[63:]))
	total := recordFixedLen + summaryLen
	if len(buf) < total {
		return Record{}, 0, io.ErrUnexpectedEOF
	}
	if declared := binary.BigEndian.Uint32(buf[27:]); int(declared) != summaryLen {
		return Record{}, 0, fmt.Errorf("journal: summary length disagreement")
	}
	want := binary.BigEndian.Uint32(buf[recordFixedLen-4:])
	crc := crc32.ChecksumIEEE(buf[:recordFixedLen-4])
	crc = crc32.Update(crc, crc32.IEEETable, buf[recordFixedLen:total])
	if want != crc {
		return Record{}, 0, fmt.Errorf("journal: crc mismatch at seq %d", binary.BigEndian.Uint64(buf[4:]))
	}
	r := Record{
		Seq:     binary.BigEndian.Uint64(buf[4:]),
		Kind:    RecordKind(buf[12]),
		Stage:   binary.BigEndian.Uint16(buf[13:]),
		TrackID: int(int32(binary.BigEndian.Uint32(buf[15:]))),
		Offset:  int64(binary.BigEndian.Uint64(buf[19:])),
		Summary: string(buf[recordFixedLen:total]),
	}
	copy(r.Digest[:], buf[31:63])
	return r, total, nil
}

// Tail describes the recoverable end of a WAL file.
type Tail struct {
	// Records is the number of valid records read.
	Records int
	// ValidBytes is the offset just past the last valid record.
	ValidBytes int64
	// TruncatedBytes is the size of an incomplete or corrupt trailing
	// record that must be discarded.
	TruncatedBytes int64
	// TruncatedReason is empty when the file ended cleanly.
	TruncatedReason string
}

// ReadAll reads records from r, stopping at a torn record. The tail is
// returned even when an error is present; err is non-nil only for I/O errors
// other than a clean EOF.
func ReadAll(r io.Reader) ([]Record, Tail, error) {
	var out []Record
	var tail Tail
	var offset int64
	hdr := make([]byte, recordFixedLen)
	for {
		n, err := io.ReadFull(r, hdr)
		offset += int64(n)
		if err == io.EOF && n == 0 {
			tail.ValidBytes = offset
			return out, tail, nil
		}
		if err == io.ErrUnexpectedEOF || (err == io.EOF && n > 0) {
			tail.TruncatedBytes = int64(n)
			tail.TruncatedReason = "partial record header"
			tail.ValidBytes = offset - int64(n)
			return out, tail, nil
		}
		if err != nil {
			return out, tail, err
		}
		summaryLen := int(binary.BigEndian.Uint16(hdr[63:]))
		summary := make([]byte, summaryLen)
		var m int
		if summaryLen > 0 {
			m, err = io.ReadFull(r, summary)
			offset += int64(m)
			if err != nil {
				tail.TruncatedBytes = int64(recordFixedLen + m)
				tail.TruncatedReason = "partial record payload"
				tail.ValidBytes = offset - tail.TruncatedBytes
				return out, tail, nil
			}
		}
		full := make([]byte, recordFixedLen+summaryLen)
		copy(full, hdr)
		copy(full[recordFixedLen:], summary)
		rec, _, decErr := Decode(full)
		if decErr != nil {
			tail.TruncatedBytes = int64(recordFixedLen + summaryLen)
			tail.TruncatedReason = decErr.Error()
			tail.ValidBytes = offset - tail.TruncatedBytes
			return out, tail, nil
		}
		out = append(out, rec)
		tail.Records++
		tail.ValidBytes = offset
	}
}

// WAL is an append-only log file.
type WAL struct {
	path string
	f    *os.File
	seq  uint64
	// size is the append offset, used to detect external truncation.
	size int64
}

// OpenWAL opens or creates path and positions the writer at the end of the
// last valid record. Any torn trailing record is truncated away.
func OpenWAL(path string) (*WAL, error) {
	w := &WAL{path: path}
	if err := w.recover(); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	w.f = f
	if err := f.Truncate(w.size); err != nil {
		f.Close()
		return nil, err
	}
	if _, err := f.Seek(w.size, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	return w, nil
}

func (w *WAL) recover() error {
	f, err := os.OpenFile(w.path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	records, tail, err := ReadAll(f)
	if err != nil {
		return err
	}
	if tail.TruncatedReason != "" && tail.TruncatedBytes > 0 {
		// A torn tail is expected after a crash; it is truncated on the
		// next append so the surviving prefix stays authoritative.
		_ = tail
	}
	if n := len(records); n > 0 {
		w.seq = records[n-1].Seq
	}
	w.size = tail.ValidBytes
	return nil
}

// Append writes one record and fsyncs it. The sequence number is assigned by
// the WAL and must be strictly increasing.
func (w *WAL) Append(r Record) (Record, error) {
	r.Seq = w.seq + 1
	buf := Encode(r)
	if _, err := w.f.WriteAt(buf, w.size); err != nil {
		return Record{}, err
	}
	if err := w.f.Sync(); err != nil {
		return Record{}, err
	}
	w.size += int64(len(buf))
	w.seq = r.Seq
	return r, nil
}

// Size returns the current append offset.
func (w *WAL) Size() int64 { return w.size }

// Path returns the log path.
func (w *WAL) Path() string { return w.path }

// Close closes the log.
func (w *WAL) Close() error { return w.f.Close() }

// Read reads every valid record from the on-disk log.
func (w *WAL) Read() ([]Record, Tail, error) {
	f, err := os.Open(w.path)
	if err != nil {
		return nil, Tail{}, err
	}
	defer f.Close()
	return ReadAll(f)
}

// TempPath returns a same-volume temporary path next to target, satisfying
// the atomic rename requirement.
func TempPath(target string) string {
	return filepath.Join(filepath.Dir(target), "."+filepath.Base(target)+".tmp")
}
