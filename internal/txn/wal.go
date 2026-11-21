// Package txn records every pipeline stage in a checksummed write-ahead log
// before committing artifacts, so an interrupted run can be resumed from the
// last atomic boundary instead of from scratch.
package txn

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
)

// WAL file layout: an 8-byte magic followed by length-framed records.
//
//	frame  = u32 bodyLen | body | u32 crc32(body)
//	body   = kind(1) | stageLen(2) | stage | seq(8) | payload
//
// A record is only visible once its length and CRC both check out, so a torn
// or corrupted tail always truncates back to a record boundary.
const (
	walMagic  = "MEDIAW1\n"
	maxRecord = 64 << 20
	frameHead = 4
	frameTail = 4
)

// Kind is a WAL record type.
type Kind uint8

const (
	// KindBegin marks the start of a stage's write.
	KindBegin Kind = 1
	// KindData carries the stage's artifacts.
	KindData Kind = 2
	// KindCommit makes a stage's data durable and reusable.
	KindCommit Kind = 3
	// KindRollback discards a stage and everything after it.
	KindRollback Kind = 4
)

// String returns the record kind's name.
func (k Kind) String() string {
	switch k {
	case KindBegin:
		return "begin"
	case KindData:
		return "data"
	case KindCommit:
		return "commit"
	case KindRollback:
		return "rollback"
	default:
		return fmt.Sprintf("kind(%d)", k)
	}
}

// Record is one scanned WAL entry.
type Record struct {
	Seq     uint64
	Kind    Kind
	Stage   string
	Payload []byte
	// Offset is the byte position of the record's frame.
	Offset int64
}

// ScanResult describes a WAL scan.
type ScanResult struct {
	Records []Record
	// ValidBytes is the length of the valid prefix.
	ValidBytes int64
	// CorruptBytes counts bytes discarded from the tail.
	CorruptBytes int64
	// CorruptReason explains why the tail was discarded, if it was.
	CorruptReason string
	// CorruptRecords counts malformed frames.
	CorruptRecords int
}

// ErrBadHeader means the WAL magic is missing, so no record can be trusted.
var ErrBadHeader = errors.New("txn: WAL header is missing or corrupt")

// WAL is an append-only, fsynced log.
type WAL struct {
	path string
	f    *os.File
	seq  uint64
	size int64
}

// OpenWAL opens (or creates) the log at path and scans its valid prefix.
func OpenWAL(path string) (*WAL, ScanResult, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, ScanResult{}, err
	}
	w := &WAL{path: path, f: f}
	res, err := w.scan()
	if err != nil {
		f.Close()
		return nil, res, err
	}
	if _, err := f.Seek(res.ValidBytes, io.SeekStart); err != nil {
		f.Close()
		return nil, res, err
	}
	w.size = res.ValidBytes
	if n := len(res.Records); n > 0 {
		w.seq = res.Records[n-1].Seq
	}
	return w, res, nil
}

// Path returns the log's file path.
func (w *WAL) Path() string { return w.path }

// LastSeq returns the sequence number of the last valid record.
func (w *WAL) LastSeq() uint64 { return w.seq }

func (w *WAL) scan() (ScanResult, error) {
	var res ScanResult
	if _, err := w.f.Seek(0, io.SeekStart); err != nil {
		return res, err
	}
	head := make([]byte, len(walMagic))
	n, err := io.ReadFull(w.f, head)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return res, err
	}
	if n == 0 {
		// Brand-new log: write the header.
		if _, err := w.f.Write([]byte(walMagic)); err != nil {
			return res, err
		}
		if err := w.f.Sync(); err != nil {
			return res, err
		}
		res.ValidBytes = int64(len(walMagic))
		return res, nil
	}
	if string(head) != walMagic {
		return res, ErrBadHeader
	}
	off := int64(len(walMagic))
	lastSeq := uint64(0)
	for {
		rec, next, reason := w.readRecord(off)
		if reason != "" {
			res.CorruptReason = reason
			res.CorruptBytes = w.fileSize() - off
			res.CorruptRecords++
			break
		}
		if rec == nil {
			break // clean EOF
		}
		if rec.Seq != lastSeq+1 {
			res.CorruptReason = fmt.Sprintf("record at %d has sequence %d, want %d", rec.Offset, rec.Seq, lastSeq+1)
			res.CorruptBytes = w.fileSize() - off
			res.CorruptRecords++
			break
		}
		lastSeq = rec.Seq
		res.Records = append(res.Records, *rec)
		off = next
	}
	res.ValidBytes = off
	return res, nil
}

func (w *WAL) fileSize() int64 {
	st, err := w.f.Stat()
	if err != nil {
		return w.size
	}
	return st.Size()
}

// readRecord reads one frame at off. A nil record with no reason means clean
// EOF; a reason means the frame is torn or checksum-invalid.
func (w *WAL) readRecord(off int64) (*Record, int64, string) {
	if _, err := w.f.Seek(off, io.SeekStart); err != nil {
		return nil, off, err.Error()
	}
	var lenBuf [4]byte
	if _, err := io.ReadFull(w.f, lenBuf[:]); err != nil {
		if err == io.EOF {
			return nil, off, ""
		}
		return nil, off, "torn frame length: " + err.Error()
	}
	bodyLen := binary.BigEndian.Uint32(lenBuf[:])
	if bodyLen == 0 || bodyLen > maxRecord {
		return nil, off, fmt.Sprintf("frame at %d declares implausible body length %d", off, bodyLen)
	}
	body := make([]byte, bodyLen)
	if _, err := io.ReadFull(w.f, body); err != nil {
		return nil, off, "torn frame body: " + err.Error()
	}
	var crcBuf [4]byte
	if _, err := io.ReadFull(w.f, crcBuf[:]); err != nil {
		return nil, off, "torn frame checksum: " + err.Error()
	}
	want := binary.BigEndian.Uint32(crcBuf[:])
	if got := crc32.ChecksumIEEE(body); got != want {
		return nil, off, fmt.Sprintf("frame at %d checksum %08x, want %08x", off, got, want)
	}
	if len(body) < 11 {
		return nil, off, fmt.Sprintf("frame at %d is too short", off)
	}
	kind := Kind(body[0])
	if kind < KindBegin || kind > KindRollback {
		return nil, off, fmt.Sprintf("frame at %d has unknown kind %d", off, kind)
	}
	stageLen := int(binary.BigEndian.Uint16(body[1:3]))
	if 3+stageLen+8 > len(body) {
		return nil, off, fmt.Sprintf("frame at %d stage length overruns body", off)
	}
	stage := string(body[3 : 3+stageLen])
	seq := binary.BigEndian.Uint64(body[3+stageLen : 11+stageLen])
	payload := append([]byte(nil), body[11+stageLen:]...)
	return &Record{
		Seq: seq, Kind: kind, Stage: stage, Payload: payload, Offset: off,
	}, off + int64(frameHead+int(bodyLen)+frameTail), ""
}

// Append writes and fsyncs one record.
func (w *WAL) Append(kind Kind, stage string, payload []byte) (Record, error) {
	if kind < KindBegin || kind > KindRollback {
		return Record{}, fmt.Errorf("txn: invalid record kind %d", kind)
	}
	if len(stage) > 0xffff {
		return Record{}, fmt.Errorf("txn: stage name longer than %d bytes", 0xffff)
	}
	body := make([]byte, 0, 11+len(stage)+len(payload))
	body = append(body, byte(kind))
	body = binary.BigEndian.AppendUint16(body, uint16(len(stage)))
	body = append(body, stage...)
	w.seq++
	seq := w.seq
	body = binary.BigEndian.AppendUint64(body, seq)
	body = append(body, payload...)
	if len(body) > maxRecord {
		return Record{}, fmt.Errorf("txn: record of %d bytes exceeds limit %d", len(body), maxRecord)
	}
	frame := make([]byte, 0, frameHead+len(body)+frameTail)
	frame = binary.BigEndian.AppendUint32(frame, uint32(len(body)))
	frame = append(frame, body...)
	frame = binary.BigEndian.AppendUint32(frame, crc32.ChecksumIEEE(body))
	if _, err := w.f.Write(frame); err != nil {
		return Record{}, err
	}
	if err := w.f.Sync(); err != nil {
		return Record{}, err
	}
	rec := Record{Seq: seq, Kind: kind, Stage: stage, Payload: payload, Offset: w.size}
	w.size += int64(len(frame))
	return rec, nil
}

// TruncateTo discards bytes past off. It is used to drop a corrupt tail.
func (w *WAL) TruncateTo(off int64) error {
	if err := w.f.Truncate(off); err != nil {
		return err
	}
	if err := w.f.Sync(); err != nil {
		return err
	}
	if _, err := w.f.Seek(off, io.SeekStart); err != nil {
		return err
	}
	w.size = off
	return nil
}

// Close closes the log.
func (w *WAL) Close() error { return w.f.Close() }
