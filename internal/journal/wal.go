// Package journal implements a write-ahead log with per-record checksums and
// an atomic, fsynced append.  A torn tail (an interrupted write) is detected
// on recovery and truncated, so a restart replays only complete records and
// never double-counts a stage.
package journal

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// Op is the kind of journaled event.
type Op string

const (
	OpBegin      Op = "begin"
	OpPrepare    Op = "prepare"
	OpCommit     Op = "commit"
	OpAbort      Op = "abort"
	OpCheckpoint Op = "checkpoint"
)

// Record is one journal entry.
type Record struct {
	Seq     uint64          `json:"seq"`
	Stage   string          `json:"stage"`
	Op      Op              `json:"op"`
	Payload json.RawMessage `json:"payload,omitempty"`
	// Digest identifies the stage artifact, letting recovery detect a payload
	// that no longer matches what was committed.
	Digest string `json:"digest,omitempty"`
}

// entry is the on-disk framing: length, checksum, then the record JSON.
type entry struct {
	length   uint32
	checksum uint32
	data     []byte
}

const (
	headerLen = 8
	// maxRecord bounds a single record so a corrupt length cannot trigger a
	// huge allocation during recovery.
	maxRecord = 8 << 20
)

// ErrTornTail reports that the final record was incomplete and discarded.
var ErrTornTail = errors.New("journal: torn tail discarded")

// WAL is a single-file write-ahead log.  All appends are fsynced before
// returning, so a record observed by a reader is durable.
type WAL struct {
	mu   sync.Mutex
	path string
	f    *os.File
	seq  uint64
	// Truncated reports how many bytes were dropped from a torn tail.
	Truncated int64
}

// Open opens or creates the WAL at path and recovers its complete records.
// The returned records are complete and ordered; any torn tail is truncated
// from the file and reported by the second error value.
func Open(path string) (*WAL, []Record, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, nil, err
	}
	recs, validEnd, torn, err := readRecords(f)
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if torn > 0 {
		if err := f.Truncate(validEnd); err != nil {
			f.Close()
			return nil, nil, err
		}
		if err := f.Sync(); err != nil {
			f.Close()
			return nil, nil, err
		}
	}
	if _, err := f.Seek(validEnd, io.SeekStart); err != nil {
		f.Close()
		return nil, nil, err
	}
	w := &WAL{path: path, f: f, Truncated: torn}
	for _, r := range recs {
		if r.Seq > w.seq {
			w.seq = r.Seq
		}
	}
	return w, recs, nil
}

// readRecords parses complete records, returning the valid prefix length and
// the number of torn bytes.
func readRecords(f *os.File) ([]Record, int64, int64, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, 0, 0, err
	}
	br := bufio.NewReaderSize(f, 64*1024)
	var out []Record
	var pos int64
	var hdr [headerLen]byte
	for {
		n, err := io.ReadFull(br, hdr[:])
		if err == io.EOF {
			return out, pos, 0, nil
		}
		if err == io.ErrUnexpectedEOF {
			// A partial header is a torn write; discard to end of file.
			tail, _ := io.ReadAll(br)
			return out, pos, int64(len(tail)) + int64(n), nil
		}
		if err != nil {
			return nil, 0, 0, err
		}
		length := binary.LittleEndian.Uint32(hdr[0:4])
		checksum := binary.LittleEndian.Uint32(hdr[4:8])
		if length == 0 || length > maxRecord {
			// Corrupt framing: treat the rest as torn rather than trusting it.
			rest, _ := io.ReadAll(br)
			return out, pos, int64(len(rest)) + headerLen, nil
		}
		data := make([]byte, length)
		if _, err := io.ReadFull(br, data); err != nil {
			// The payload itself was cut short: everything from pos is torn.
			got, _ := io.ReadAll(br)
			return out, pos, int64(len(data)) + int64(len(got)) + headerLen, nil
		}
		if crc32.ChecksumIEEE(data) != checksum {
			rest, _ := io.ReadAll(br)
			return out, pos, int64(length) + int64(len(rest)) + headerLen, nil
		}
		var r Record
		if err := json.Unmarshal(data, &r); err != nil {
			rest, _ := io.ReadAll(br)
			return out, pos, int64(length) + int64(len(rest)) + headerLen, nil
		}
		out = append(out, r)
		pos += headerLen + int64(length)
	}
}

// Append writes and fsyncs one record, assigning the next sequence number.
func (w *WAL) Append(r Record) (Record, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.seq++
	r.Seq = w.seq
	data, err := json.Marshal(r)
	if err != nil {
		return Record{}, err
	}
	if len(data) > maxRecord {
		return Record{}, fmt.Errorf("journal: record of %d bytes exceeds limit", len(data))
	}
	var hdr [headerLen]byte
	binary.LittleEndian.PutUint32(hdr[0:4], uint32(len(data)))
	binary.LittleEndian.PutUint32(hdr[4:8], crc32.ChecksumIEEE(data))
	if _, err := w.f.Write(hdr[:]); err != nil {
		return Record{}, err
	}
	if _, err := w.f.Write(data); err != nil {
		return Record{}, err
	}
	if err := w.f.Sync(); err != nil {
		return Record{}, err
	}
	return r, nil
}

// ReadAll re-reads the complete records from disk.
func (w *WAL) ReadAll() ([]Record, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	recs, _, _, err := readRecords(w.f)
	return recs, err
}

// Close closes the underlying file.
func (w *WAL) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}

// Path returns the WAL file path.
func (w *WAL) Path() string { return w.path }

// TruncateAt simulates an interrupted write by cutting the file at offset,
// used by fault injection to create a torn tail.
func TruncateAt(path string, offset int64) error {
	return os.Truncate(path, offset)
}

// Stages folds records into per-stage state, where the last terminal op wins.
func Stages(recs []Record) map[string]Record {
	out := make(map[string]Record)
	for _, r := range recs {
		switch r.Op {
		case OpBegin, OpPrepare, OpCommit, OpAbort:
			out[r.Stage] = r
		}
	}
	return out
}
