package journal

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// MaxRecords bounds the journal size (日志空间约束).
const MaxRecords = 10000

// Record types written to the WAL.
const (
	TypeStage     = "stage"     // stage input/output digests
	TypeBegin     = "begin"     // transaction begin, before any write
	TypeCommitted = "committed" // transaction durable
	TypeRollback  = "rollback"  // incomplete transaction rolled back
)

// Record is one append-only WAL entry.
type Record struct {
	Seq    int    `json:"seq"`
	Type   string `json:"type"`
	Stage  string `json:"stage,omitempty"`
	Txn    int    `json:"txn,omitempty"`
	Input  string `json:"input,omitempty"`
	Output string `json:"output,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// WAL is a write-ahead log: every record is flushed and fsynced before
// the operation it describes is considered started.
type WAL struct {
	mu      sync.Mutex
	path    string
	f       *os.File
	records []Record
}

// Open opens (or creates) the WAL in dir and replays existing records.
func Open(dir string) (*WAL, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "wal.log")
	w := &WAL{path: path}
	if f, err := os.Open(path); err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
		for sc.Scan() {
			line := sc.Bytes()
			if len(line) == 0 {
				continue
			}
			var r Record
			if err := json.Unmarshal(line, &r); err != nil {
				f.Close()
				return nil, fmt.Errorf("journal: corrupt record %d: %w", len(w.records)+1, err)
			}
			w.records = append(w.records, r)
		}
		if err := sc.Err(); err != nil {
			f.Close()
			return nil, err
		}
		f.Close()
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	w.f = f
	return w, nil
}

// Append writes one record and fsyncs before returning.
func (w *WAL) Append(r Record) (Record, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.records) >= MaxRecords {
		return Record{}, fmt.Errorf("journal: record limit %d reached", MaxRecords)
	}
	r.Seq = len(w.records) + 1
	b, err := json.Marshal(r)
	if err != nil {
		return Record{}, err
	}
	if _, err := w.f.Write(append(b, '\n')); err != nil {
		return Record{}, err
	}
	if err := w.f.Sync(); err != nil {
		return Record{}, err
	}
	w.records = append(w.records, r)
	return r, nil
}

// Records returns a copy of all replayed and appended records.
func (w *WAL) Records() []Record {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]Record, len(w.records))
	copy(out, w.records)
	return out
}

// LastTxn returns the newest begin record and whether a matching
// committed record exists after it.
func (w *WAL) LastTxn() (begin Record, committed bool, found bool) {
	recs := w.Records()
	for i := len(recs) - 1; i >= 0; i-- {
		if recs[i].Type == TypeBegin {
			begin, found = recs[i], true
			for _, r := range recs[i+1:] {
				if r.Type == TypeCommitted && r.Txn == begin.Txn {
					committed = true
				}
			}
			return
		}
	}
	return
}

// StageDone reports whether a stage record with the given input digest
// was already written — replay uses this to avoid duplicate commits.
func (w *WAL) StageDone(stage, input string) bool {
	for _, r := range w.Records() {
		if r.Type == TypeStage && r.Stage == stage && r.Input == input {
			return true
		}
	}
	return false
}

// Close flushes and closes the WAL file.
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
