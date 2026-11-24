// Package txn provides checksummed write-ahead logging, checkpoints, atomic
// file commit, and resume/recovery semantics.
package txn

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"strings"

	"mediad/internal/fault"
)

// WALFileName and CheckpointFileName are stable on-disk names.
const (
	WALFileName        = "mediad.wal"
	CheckpointFileName = "checkpoint.json"
	Version            = 1
)

// Entry is one checksummed write-ahead log record.
type Entry struct {
	LSN      int64  `json:"lsn"`
	TxID     string `json:"tx_id"`
	Stage    string `json:"stage"`
	Op       string `json:"op"` // begin, prepared, commit, rollback
	Output   string `json:"output,omitempty"`
	SHA256   string `json:"sha256,omitempty"`
	Bytes    int64  `json:"bytes,omitempty"`
	Checksum uint32 `json:"checksum"`
}

func (e Entry) canonical() ([]byte, error) {
	e.Checksum = 0
	return json.Marshal(e)
}

func (e *Entry) sign() error {
	b, err := e.canonical()
	if err != nil {
		return err
	}
	e.Checksum = crc32.ChecksumIEEE(b)
	return nil
}

func (e *Entry) verify() error {
	want := e.Checksum
	if err := e.sign(); err != nil {
		return err
	}
	if e.Checksum != want {
		return fmt.Errorf("WAL checksum mismatch at LSN %d", e.LSN)
	}
	return nil
}

// WAL is an append-only JSON-line log with per-entry CRC32.
type WAL struct {
	path string
	f    *os.File
	lsn  int64
}

func OpenWAL(dir string) (*WAL, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, WALFileName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	w := &WAL{path: path, f: f}
	entries, validBytes, err := readWAL(path)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if len(entries) > 0 {
		w.lsn = entries[len(entries)-1].LSN
	}
	// Remove a torn/corrupt tail so future appends start at a valid boundary.
	if err := f.Truncate(validBytes); err != nil {
		_ = f.Close()
		return nil, err
	}
	if _, err := f.Seek(validBytes, io.SeekStart); err != nil {
		_ = f.Close()
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		_ = f.Close()
		return nil, err
	}
	return w, nil
}

func readWAL(path string) ([]Entry, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, nil
		}
		return nil, 0, err
	}
	defer f.Close()

	var entries []Entry
	var validBytes int64
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		var e Entry
		if err := json.Unmarshal(line, &e); err != nil {
			break
		}
		if err := e.verify(); err != nil {
			break
		}
		entries = append(entries, e)
		validBytes += int64(len(line)) + 1 // newline
	}
	if err := sc.Err(); err != nil {
		return entries, validBytes, err
	}
	return entries, validBytes, nil
}

// ReadWALEntries is used by recovery and tests.
func ReadWALEntries(dir string) ([]Entry, error) {
	entries, _, err := readWAL(filepath.Join(dir, WALFileName))
	return entries, err
}

func (w *WAL) Append(txID, stage, op, output, sha string, n int64) (Entry, error) {
	w.lsn++
	e := Entry{LSN: w.lsn, TxID: txID, Stage: stage, Op: op, Output: output, SHA256: sha, Bytes: n}
	if err := e.sign(); err != nil {
		return e, err
	}
	b, err := json.Marshal(e)
	if err != nil {
		return e, err
	}
	if _, err := w.f.Write(append(b, '\n')); err != nil {
		return e, err
	}
	if err := w.f.Sync(); err != nil {
		return e, err
	}
	return e, nil
}

func (w *WAL) Close() error { return w.f.Close() }

// SHA256File returns the lowercase hex SHA-256 of a file.
func SHA256File(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", n, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// SHA256Bytes returns the lowercase hex digest of b.
func SHA256Bytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// AtomicWrite writes to a same-directory temporary file and renames it.
// It never truncates an existing destination before the replacement is ready.
func AtomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return err
	}
	return nil
}

// IsExitInterrupted is a small convenience for callers.
func IsExitInterrupted(err error) bool {
	return fault.ExitCode(err) == fault.ExitInterrupted
}

// JoinOutput keeps output paths relative to the transaction directory unless
// they are already absolute.
func JoinOutput(dir, name string) string {
	if filepath.IsAbs(name) {
		return name
	}
	return filepath.Join(dir, name)
}

// StableTxID returns the fixed transaction identity for one input seed.
func StableTxID(parts ...string) string {
	h := sha256.New()
	h.Write([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(h.Sum(nil))[:16]
}
