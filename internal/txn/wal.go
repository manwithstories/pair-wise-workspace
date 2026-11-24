// Package txn provides a write-ahead log and atomic segment commits. Every
// stage records intent before acting; recovery replays the log so an
// interrupted run converges on the same committed state as a clean run.
package txn

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"mediarun/internal/model"
)

// Large records are rejected rather than partially applied.
const (
	MaxRecordBytes = 1 << 20
	MaxRecords     = 100000
)

// Op identifies the intent of a WAL record.
type Op string

const (
	OpBegin    Op = "begin"
	OpStage    Op = "stage"
	OpCommit   Op = "commit"
	OpRollback Op = "rollback"
	OpCheck    Op = "check"
)

// Record is one durable log entry.
type Record struct {
	Seq      uint64 `json:"seq"`
	Op       Op     `json:"op"`
	Segment  string `json:"segment,omitempty"`
	Stage    string `json:"stage,omitempty"`
	Hash     string `json:"hash,omitempty"`
	Detail   string `json:"detail,omitempty"`
	Checksum string `json:"checksum"`
}

// CRCDetail returns the checksum of everything except the checksum field.
func (r Record) computeChecksum() string {
	h := sha256.New()
	fmt.Fprintf(h, "%d|%s|%s|%s|%s|%s", r.Seq, r.Op, r.Segment, r.Stage, r.Hash, r.Detail)
	return hex.EncodeToString(h.Sum(nil))
}

// WAL is a single append-only log file.
type WAL struct {
	dir    string
	path   string
	file   *os.File
	seq    uint64
	rows   int
	closed bool
	sync   bool
}

// Open creates or opens the log in dir. Existing records are counted so the
// sequence numbers continue; a partially written tail left by an earlier
// crash is cut away first so the new records stay parseable.
func Open(dir string) (*WAL, error) {
	if dir == "" {
		return nil, fmt.Errorf("%w: wal directory", model.ErrInvalid)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "wal.jsonl")
	records, truncated, err := Replay(dir)
	if err != nil {
		return nil, err
	}
	if truncated {
		if err := trimPartialTail(path); err != nil {
			return nil, err
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	return &WAL{dir: dir, path: path, file: f, seq: uint64(len(records)), rows: len(records), sync: true}, nil
}

// trimPartialTail cuts the file back to its last newline.
func trimPartialTail(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	cut := bytes.LastIndexByte(data, '\n')
	keep := int64(0)
	if cut >= 0 {
		keep = int64(cut) + 1
	}
	return os.Truncate(path, keep)
}

// Path returns the log file location.
func (w *WAL) Path() string { return w.path }

// Append writes one record with fsync so a crash after Append still leaves a
// recoverable intent on disk.
func (w *WAL) Append(r Record) error {
	if w.closed {
		return fmt.Errorf("%w: wal closed", model.ErrInvalid)
	}
	if r.Op == "" {
		return fmt.Errorf("%w: empty op", model.ErrInvalid)
	}
	if w.rows >= MaxRecords {
		return fmt.Errorf("%w: wal record limit", model.ErrLimit)
	}
	r.Seq = w.seq + 1
	if r.Checksum != "" {
		return fmt.Errorf("%w: caller may not set checksum", model.ErrInvalid)
	}
	r.Checksum = r.computeChecksum()
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if len(data)+1 > MaxRecordBytes {
		return fmt.Errorf("%w: wal record %d bytes", model.ErrLimit, len(data))
	}
	if _, err := w.file.Write(append(data, '\n')); err != nil {
		return err
	}
	if w.sync {
		if err := w.file.Sync(); err != nil {
			return err
		}
	}
	w.seq = r.Seq
	w.rows++
	return nil
}

// Seq is the last sequence number written.
func (w *WAL) Seq() uint64 { return w.seq }

// Close closes the file once.
func (w *WAL) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	return w.file.Close()
}

// Replay reads the log and returns every valid record in order. A final
// partial line (a crash mid-write) is tolerated and reported through
// truncated=true; damage anywhere else is a corruption error.
func Replay(dir string) (records []Record, truncated bool, err error) {
	path := filepath.Join(dir, "wal.jsonl")
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	defer f.Close()

	reader := bufio.NewReaderSize(f, 64<<10)
	for i := 0; ; i++ {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] != '\n' {
			// No trailing newline: the writer died mid-record. Everything
			// before this line is intact.
			return records, true, nil
		}
		if len(line) == 0 && readErr == io.EOF {
			return records, false, nil
		}
		if readErr != nil && readErr != io.EOF {
			return records, false, readErr
		}
		line = line[:len(line)-1]
		if len(line) == 0 {
			continue
		}
		if len(line) > MaxRecordBytes {
			return records, false, fmt.Errorf("%w: wal line %d too long", model.ErrLimit, i)
		}
		var r Record
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&r); err != nil {
			return records, false, fmt.Errorf("%w: wal line %d: %v", model.ErrCorruptIndex, i+1, err)
		}
		if r.Checksum != r.computeChecksum() {
			return records, false, fmt.Errorf("%w: wal line %d checksum mismatch", model.ErrCorruptIndex, i+1)
		}
		if r.Seq != uint64(len(records))+1 {
			return records, false, fmt.Errorf("%w: wal line %d sequence %d", model.ErrCorruptIndex, i+1, r.Seq)
		}
		records = append(records, r)
		if len(records) > MaxRecords {
			return records, false, fmt.Errorf("%w: wal record limit", model.ErrLimit)
		}
		if readErr == io.EOF {
			return records, false, nil
		}
	}
}

// Reset removes the log, used when an artifacts directory is rebuilt from
// scratch.
func Reset(dir string) error {
	err := os.Remove(filepath.Join(dir, "wal.jsonl"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
