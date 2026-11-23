// Package journal implements a bounded write-ahead log recording stage
// input digests, completion status and commit boundaries.
package journal

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

// Status of a stage record.
const (
	StatusBegin  = "begin"
	StatusDone   = "done"
	StatusCommit = "commit"
)

// Record is one WAL entry.
type Record struct {
	Seq       int    `json:"seq"`
	Stage     string `json:"stage"`
	Status    string `json:"status"`
	InputSHA  string `json:"input_sha"`
	OutputSHA string `json:"output_sha"`
}

// WAL is an append-only log with bounded growth: when the record count
// exceeds MaxRecords the log is compacted to committed records only.
type WAL struct {
	path       string
	MaxRecords int
	recs       []Record
	f          *os.File
}

// Open loads (or creates) the WAL at path.
func Open(path string, maxRecords int) (*WAL, error) {
	if maxRecords <= 0 {
		maxRecords = 1024
	}
	w := &WAL{path: path, MaxRecords: maxRecords}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	w.f = f
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var r Record
		if err := json.Unmarshal(line, &r); err != nil {
			return nil, fmt.Errorf("wal: corrupt record: %w", err)
		}
		w.recs = append(w.recs, r)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return nil, err
	}
	return w, nil
}

// Close flushes and closes the log.
func (w *WAL) Close() error { return w.f.Close() }

// Records returns a copy of all records.
func (w *WAL) Records() []Record { return append([]Record{}, w.recs...) }

// Committed returns the commit record for a stage, if any.
func (w *WAL) Committed(stage string) (Record, bool) {
	for i := len(w.recs) - 1; i >= 0; i-- {
		if w.recs[i].Stage == stage && w.recs[i].Status == StatusCommit {
			return w.recs[i], true
		}
	}
	return Record{}, false
}

// Append writes one record and fsyncs. seq must be monotonic.
func (w *WAL) Append(r Record) error {
	r.Seq = len(w.recs)
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if _, err := w.f.Write(append(b, '\n')); err != nil {
		return err
	}
	if err := w.f.Sync(); err != nil {
		return err
	}
	w.recs = append(w.recs, r)
	if len(w.recs) > w.MaxRecords {
		return w.compact()
	}
	return nil
}

// compact rewrites the log keeping only the latest commit per stage plus
// any records after the last commit (in-flight work).
func (w *WAL) compact() error {
	lastCommit := map[string]int{}
	for i, r := range w.recs {
		if r.Status == StatusCommit {
			lastCommit[r.Stage] = i
		}
	}
	var keep []Record
	for i, r := range w.recs {
		if r.Status == StatusCommit && lastCommit[r.Stage] == i {
			keep = append(keep, r)
		}
	}
	if len(keep) == len(w.recs) {
		return errors.New("wal: cannot compact below limit; too many stages")
	}
	tmp := w.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	for i, r := range keep {
		r.Seq = i
		b, _ := json.Marshal(r)
		if _, err := f.Write(append(b, '\n')); err != nil {
			f.Close()
			return err
		}
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, w.path); err != nil {
		return err
	}
	w.f.Close()
	nf, err := os.OpenFile(w.path, os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	w.f = nf
	w.recs = keep
	return nil
}
