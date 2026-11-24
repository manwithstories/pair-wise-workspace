// Package txn implements the write-ahead log, staging generations, atomic
// commit markers and recovery/rollback for interrupted batches.
package txn

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"bcast/internal/media"
)

// WALEntry is one JSONL write-ahead record.
type WALEntry struct {
	Stage      string `json:"stage"`
	Generation string `json:"generation"`
	Status     string `json:"status"`
	Summary    string `json:"summary,omitempty"`
	SHA256     string `json:"sha256,omitempty"`
}

// WAL is an append-only JSONL write-ahead log.
type WAL struct {
	path string
	mu   sync.Mutex
	f    *os.File
}

// OpenWAL creates or opens out/wal.log and ensures out exists.
func OpenWAL(out string) (*WAL, error) {
	if err := os.MkdirAll(out, 0o755); err != nil {
		return nil, media.IO("wal", out, 0, 0, err)
	}
	path := filepath.Join(out, "wal.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, media.IO("wal", path, 0, 0, err)
	}
	return &WAL{path: path, f: f}, nil
}

// Path returns the WAL file path.
func (w *WAL) Path() string { return w.path }

// Append writes and fsyncs one stage record.
func (w *WAL) Append(e WALEntry) error {
	if w == nil || w.f == nil {
		return media.IO("wal", w.path, 0, 0, os.ErrInvalid)
	}
	b, err := json.Marshal(e)
	if err != nil {
		return media.Corrupt("wal", "encode_entry", w.path, 0, -1, "%v", err)
	}
	b = append(b, '\n')
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, err := w.f.Write(b); err != nil {
		return media.IO("wal", w.path, 0, 0, err)
	}
	if err := w.f.Sync(); err != nil {
		return media.IO("wal", w.path, 0, 0, err)
	}
	return nil
}

// Close syncs and closes the log.
func (w *WAL) Close() error {
	if w == nil || w.f == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.f.Sync(); err != nil {
		_ = w.f.Close()
		return media.IO("wal", w.path, 0, 0, err)
	}
	if err := w.f.Close(); err != nil {
		return media.IO("wal", w.path, 0, 0, err)
	}
	w.f = nil
	return nil
}

// ReadWAL reads all valid JSONL entries.  A torn final line is reported as a
// locatable WAL fault rather than silently discarded.
func ReadWAL(out string) ([]WALEntry, error) {
	path := filepath.Join(out, "wal.log")
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, media.IO("wal", path, 0, 0, err)
	}
	var entries []WALEntry
	start := 0
	lineNo := 0
	for i, c := range b {
		if c != '\n' {
			continue
		}
		lineNo++
		line := b[start:i]
		start = i + 1
		if len(line) == 0 {
			continue
		}
		var e WALEntry
		if err := json.Unmarshal(line, &e); err != nil {
			return entries, media.Corrupt("wal", "invalid_wal_line", path, 0, int64(start), "line %d: %v", lineNo, err)
		}
		entries = append(entries, e)
	}
	if start < len(b) {
		return entries, media.Corrupt("wal", "torn_wal_line", path, 0, int64(start), "final record has no newline")
	}
	return entries, nil
}

// WriteFileAtomic writes data to name.tmp, fsyncs it and renames it to name.
// The target is never observable as a partially written file.
func WriteFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return media.IO("write", dir, 0, 0, err)
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return media.IO("write", path, 0, 0, err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return media.IO("write", path, 0, 0, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return media.IO("write", path, 0, 0, err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return media.IO("write", path, 0, 0, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return media.IO("write", path, 0, 0, err)
	}
	return nil
}
