// Package txn implements crash-safe transactional output: samples are first
// staged with write-ahead logging, every staged byte is checksum-verified,
// and only then is the CURRENT commit boundary switched atomically.
package txn

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// WAL record types.
const (
	RecBegin    byte = 'B' // transaction started; payload: commit id
	RecStage    byte = 'S' // one sample staged; payload: StageRecord
	RecManifest byte = 'M' // manifest staged; payload: manifest bytes
	RecCommit   byte = 'C' // commit boundary switched; payload: commit id
	RecRollback byte = 'R' // transaction discarded; payload: commit id
	RecEnd      byte = 'E' // transaction closed cleanly; payload: commit id
)

var walMagic = [4]byte{'W', 'A', 'L', '1'}

const (
	walHdrSize  = 4 + 1 + 4 // magic, type, payload length
	walHashSize = 32
)

// StageRecord is the payload of a RecStage entry: the offset, size and hash
// recorded for every staged sample, plus its path relative to the work dir.
type StageRecord struct {
	TrackID  uint32
	Sequence uint64
	Offset   uint64
	Size     uint32
	SHA256   [32]byte
	RelPath  string
}

// Record is one parsed WAL entry with its absolute byte offset in wal.log.
type Record struct {
	Type    byte
	Offset  int64
	Payload []byte
}

// WAL is an append-only, fsync'd write-ahead log. Reads validate each
// record's checksum so a torn or corrupted entry is detected during replay.
type WAL struct {
	path string
	f    *os.File
}

// OpenWAL opens (creating if needed) the log inside dir.
func OpenWAL(dir string) (*WAL, error) {
	if err := ensureDir(dir); err != nil {
		return nil, err
	}
	p := filepath.Join(dir, "wal.log")
	f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	return &WAL{path: p, f: f}, nil
}

// Path returns the log file location.
func (w *WAL) Path() string { return w.path }

// Append writes one record, fsyncs it and returns its starting offset.
func (w *WAL) Append(recType byte, payload []byte) (int64, error) {
	off, err := w.f.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, err
	}
	hdr := make([]byte, walHdrSize)
	copy(hdr[0:4], walMagic[:])
	hdr[4] = recType
	binary.BigEndian.PutUint32(hdr[5:9], uint32(len(payload)))
	hasher := sha256.New()
	hasher.Write(hdr)
	hasher.Write(payload)
	sum := hasher.Sum(nil)
	buf := make([]byte, 0, len(hdr)+len(payload)+walHashSize)
	buf = append(buf, hdr...)
	buf = append(buf, payload...)
	buf = append(buf, sum...)
	if _, err := w.f.Write(buf); err != nil {
		return 0, err
	}
	if err := w.f.Sync(); err != nil {
		return 0, err
	}
	return off, nil
}

// ReadAll replays and checksum-verifies every record in the log.
func (w *WAL) ReadAll() ([]Record, error) {
	if _, err := w.f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(w.f)
	if err != nil {
		return nil, err
	}
	var out []Record
	pos := 0
	for pos < len(data) {
		if len(data)-pos < walHdrSize {
			return nil, errors.New("wal: truncated record header")
		}
		var magic [4]byte
		copy(magic[:], data[pos:pos+4])
		if magic != walMagic {
			return nil, errors.New("wal: bad magic")
		}
		recType := data[pos+4]
		payloadLen := int(binary.BigEndian.Uint32(data[pos+5 : pos+9]))
		end := pos + walHdrSize + payloadLen
		if end+walHashSize > len(data) {
			return nil, errors.New("wal: truncated record payload")
		}
		payload := data[pos+walHdrSize : end]
		stored := data[end : end+walHashSize]
		hasher := sha256.New()
		hasher.Write(data[pos:end])
		var want [walHashSize]byte
		copy(want[:], hasher.Sum(nil))
		var got [walHashSize]byte
		copy(got[:], stored)
		if want != got {
			return nil, errors.New("wal: record checksum mismatch")
		}
		out = append(out, Record{Type: recType, Offset: int64(pos), Payload: append([]byte(nil), payload...)})
		pos = end + walHashSize
	}
	return out, nil
}

// Close releases the underlying file.
func (w *WAL) Close() error { return w.f.Close() }

// EncodeStage serialises a StageRecord payload.
func EncodeStage(s StageRecord) []byte {
	name := []byte(s.RelPath)
	b := make([]byte, 8+8+8+4+32+2+len(name))
	bin := binary.BigEndian
	bin.PutUint32(b[0:4], s.TrackID)
	bin.PutUint64(b[4:12], s.Sequence)
	bin.PutUint64(b[12:20], s.Offset)
	bin.PutUint32(b[20:24], s.Size)
	copy(b[24:56], s.SHA256[:])
	bin.PutUint16(b[56:58], uint16(len(name)))
	copy(b[58:], name)
	return b
}

// DecodeStage parses a StageRecord payload.
func DecodeStage(b []byte) (StageRecord, error) {
	if len(b) < 58 {
		return StageRecord{}, errors.New("wal: short stage record")
	}
	nameLen := int(binary.BigEndian.Uint16(b[56:58]))
	if 58+nameLen != len(b) {
		return StageRecord{}, errors.New("wal: malformed stage record")
	}
	s := StageRecord{
		TrackID:  binary.BigEndian.Uint32(b[0:4]),
		Sequence: binary.BigEndian.Uint64(b[4:12]),
		Offset:   binary.BigEndian.Uint64(b[12:20]),
		Size:     binary.BigEndian.Uint32(b[20:24]),
		RelPath:  string(b[58 : 58+nameLen]),
	}
	copy(s.SHA256[:], b[24:56])
	return s, nil
}

func ensureDir(dir string) error {
	info, err := os.Stat(dir)
	if os.IsNotExist(err) {
		return os.MkdirAll(dir, 0o755)
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("txn: %s is not a directory", dir)
	}
	return nil
}
