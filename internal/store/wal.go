// Package store provides a write-ahead log and atomic stage commits so a
// crash before commit rolls back uncommitted artifacts and a restart
// resumes idempotently.
package store

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Entry is one WAL record, one JSON object per line.
type Entry struct {
	Op    string `json:"op"` // "stage" or "commit"
	Stage string `json:"stage"`
	Hash  string `json:"hash,omitempty"`
}

// Recovery is the replayed WAL state at open time.
type Recovery struct {
	Committed []string // stages with a commit record
	Pending   []string // stages appended but never committed
	Truncated bool     // a trailing partial line was ignored
}

// WAL appends stage/commit records to dir/wal.log.
type WAL struct {
	path string
	f    *os.File
}

// validStage rejects path traversal in stage names.
func validStage(stage string) error {
	if stage == "" || strings.ContainsAny(stage, `/\`) ||
		strings.Contains(stage, "..") {
		return fmt.Errorf("store: invalid stage name %q", stage)
	}
	return nil
}

// OpenWAL opens or creates the WAL in dir and replays it.
func OpenWAL(dir string) (*WAL, *Recovery, error) {
	path := filepath.Join(dir, "wal.log")
	rec := &Recovery{}
	if data, err := os.ReadFile(path); err == nil {
		replay(data, rec)
	} else if !os.IsNotExist(err) {
		return nil, nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, nil, err
	}
	return &WAL{path: path, f: f}, rec, nil
}

func replay(data []byte, rec *Recovery) {
	committed := map[string]bool{}
	var pending []string
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	var lines []string
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	for i, ln := range lines {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		var e Entry
		if err := json.Unmarshal([]byte(ln), &e); err != nil {
			if i == len(lines)-1 {
				rec.Truncated = true // torn tail write: ignore
				continue
			}
			continue // mid-file corruption is skipped, not fatal
		}
		switch e.Op {
		case "stage":
			if !committed[e.Stage] {
				pending = append(pending, e.Stage)
			}
		case "commit":
			committed[e.Stage] = true
			next := pending[:0]
			for _, p := range pending {
				if p != e.Stage {
					next = append(next, p)
				}
			}
			pending = next
		}
	}
	for s := range committed {
		rec.Committed = append(rec.Committed, s)
	}
	rec.Pending = pending
}

func (w *WAL) append(e Entry) error {
	if err := validStage(e.Stage); err != nil {
		return err
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if _, err := w.f.Write(append(b, '\n')); err != nil {
		return err
	}
	return w.f.Sync()
}

// AppendStage records that a stage produced an artifact with the given hash.
func (w *WAL) AppendStage(stage, hash string) error {
	return w.append(Entry{Op: "stage", Stage: stage, Hash: hash})
}

// Commit records that a stage's artifact is durable.
func (w *WAL) Commit(stage string) error {
	return w.append(Entry{Op: "commit", Stage: stage})
}

// Close flushes and closes the WAL.
func (w *WAL) Close() error { return w.f.Close() }
