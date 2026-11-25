// Package txn provides a fsynced write-ahead log, per-segment atomic fragment
// commit and an atomic manifest. Recovery only replays durably recorded intent
// or discards staging files; it never invents committed data.
package txn

import (
	"bufio"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"relaycheck/internal/media"
)

// Phase labels WAL records.
type Phase string

const (
	PhaseBegin     Phase = "begin"
	PhasePrepared  Phase = "prepared"
	PhaseCommitted Phase = "committed"
	PhaseManifest  Phase = "manifest"
)

// Record is one WAL entry.
type Record struct {
	LSN     int64  `json:"lsn"`
	Phase   Phase  `json:"phase"`
	Segment int    `json:"segment"`
	SHA256  string `json:"sha256,omitempty"`
	Size    int64  `json:"size,omitempty"`
}

// WAL is an append-only, fsynced JSON-line log.
type WAL struct {
	mu   sync.Mutex
	path string
	f    *os.File
	lsn  int64
}

// OpenWAL creates the directory and opens (or creates) wal.log.
func OpenWAL(dir string) (*WAL, *media.Fault) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, media.NewFault(media.CodeCommitFailed, "create txn dir: %v", err)
	}
	path := filepath.Join(dir, "wal.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return nil, media.NewFault(media.CodeCommitFailed, "open wal: %v", err)
	}
	w := &WAL{path: path, f: f}
	recs, rf := w.ReadAll()
	if rf != nil && rf.Code != media.CodeWALCorrupt {
		_ = f.Close()
		return nil, rf
	}
	for _, r := range recs {
		if r.LSN > w.lsn {
			w.lsn = r.LSN
		}
	}
	return w, nil
}

// Path returns the WAL file path.
func (w *WAL) Path() string { return w.path }

// Append writes and fsyncs one record before returning.
func (w *WAL) Append(r Record) (Record, *media.Fault) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lsn++
	r.LSN = w.lsn
	b, err := json.Marshal(r)
	if err != nil {
		return r, media.NewFault(media.CodeCommitFailed, "marshal wal record: %v", err)
	}
	if _, err := w.f.Write(append(b, '\n')); err != nil {
		return r, media.NewFault(media.CodeCommitFailed, "write wal: %v", err)
	}
	if err := w.f.Sync(); err != nil {
		return r, media.NewFault(media.CodeCommitFailed, "fsync wal: %v", err)
	}
	return r, nil
}

// Sync flushes the WAL file.
func (w *WAL) Sync() *media.Fault {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.f.Sync(); err != nil {
		return media.NewFault(media.CodeCommitFailed, "sync wal: %v", err)
	}
	return nil
}

// Close syncs and closes the WAL.
func (w *WAL) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.f.Sync()
	return w.f.Close()
}

// ReadAll parses every WAL line. A malformed record is WALCorrupt.
func (w *WAL) ReadAll() ([]Record, *media.Fault) {
	f, err := os.Open(w.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, media.NewFault(media.CodeWALCorrupt, "reopen wal: %v", err)
	}
	defer f.Close()
	var out []Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	line := 0
	for sc.Scan() {
		line++
		raw := sc.Bytes()
		if len(raw) == 0 {
			continue
		}
		var r Record
		if err := json.Unmarshal(raw, &r); err != nil {
			return out, media.NewFault(media.CodeWALCorrupt, "wal line %d: %v", line, err)
		}
		out = append(out, r)
	}
	if err := sc.Err(); err != nil {
		return out, media.NewFault(media.CodeWALCorrupt, "scan wal: %v", err)
	}
	return out, nil
}

// SyncDir fsyncs a directory so a rename is durable. Best effort on platforms
// that do not support syncing directories.
func SyncDir(dir string) *media.Fault {
	d, err := os.Open(dir)
	if err != nil {
		return media.NewFault(media.CodeCommitFailed, "open dir for sync: %v", err)
	}
	err = d.Sync()
	_ = d.Close()
	if err != nil {
		return media.NewFault(media.CodeCommitFailed, "sync dir: %v", err)
	}
	return nil
}

// SortedDirEntries is a small deterministic directory listing helper.
func SortedDirEntries(dir string) ([]fs.DirEntry, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	return entries, nil
}
