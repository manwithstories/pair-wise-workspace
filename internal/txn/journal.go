// Package txn implements a write-ahead journal, atomic commits and
// crash recovery so that pipeline stages can resume idempotently.
package txn

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/example/mediad/internal/model"
)

// Stage names recorded in the journal.
const (
	StagePlan   = "plan"
	StageWrite  = "write"
	StageVerify = "verify"
	StageCommit = "commit"
)

// Entry is one write-ahead log record.
type Entry struct {
	Seq        int    `json:"seq"`
	Stage      string `json:"stage"`
	SegmentID  string `json:"segment_id"`
	State      string `json:"state"` // begin | done
	InputHash  string `json:"input_hash,omitempty"`
	OutputPath string `json:"output_path,omitempty"`
	OutputHash string `json:"output_hash,omitempty"`
}

// Journal is an append-only, fsync-backed write-ahead log.
type Journal struct {
	mu   sync.Mutex
	path string
	f    *os.File
	seq  int
}

// OpenJournal opens (or creates) the journal and replays its entries.
func OpenJournal(dir string) (*Journal, []Entry, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, nil, model.Wrap(model.KindState, "txn", "journal", dir, err)
	}
	path := filepath.Join(dir, "journal.jsonl")
	entries, err := readEntries(path)
	if err != nil {
		return nil, nil, err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, nil, model.Wrap(model.KindState, "txn", "journal", path, err)
	}
	j := &Journal{path: path, f: f}
	if len(entries) > 0 {
		j.seq = entries[len(entries)-1].Seq
	}
	return j, entries, nil
}

func readEntries(path string) ([]Entry, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, model.Wrap(model.KindState, "txn", "journal", path, err)
	}
	defer f.Close()

	var entries []Entry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(line, &e); err != nil {
			// A torn final line from a crash: stop replay at the last
			// complete boundary.
			return entries, model.Wrap(model.KindState, "txn", "journal",
				path, err)
		}
		entries = append(entries, e)
	}
	return entries, sc.Err()
}

// Append writes one entry and flushes it to disk before returning.
func (j *Journal) Append(e Entry) (Entry, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.seq++
	e.Seq = j.seq
	line, err := json.Marshal(e)
	if err != nil {
		return e, model.Wrap(model.KindState, "txn", "journal", j.path, err)
	}
	line = append(line, '\n')
	if _, err := j.f.Write(line); err != nil {
		return e, model.Wrap(model.KindState, "txn", "journal", j.path, err)
	}
	if err := j.f.Sync(); err != nil {
		return e, model.Wrap(model.KindState, "txn", "journal", j.path, err)
	}
	return e, nil
}

// Begin records the start boundary of a stage.
func (j *Journal) Begin(stage, segmentID, inputHash string) (Entry, error) {
	return j.Append(Entry{Stage: stage, SegmentID: segmentID,
		State: "begin", InputHash: inputHash})
}

// Done records the complete boundary of a stage.
func (j *Journal) Done(stage, segmentID, outputPath, outputHash string) (Entry, error) {
	return j.Append(Entry{Stage: stage, SegmentID: segmentID,
		State: "done", OutputPath: outputPath, OutputHash: outputHash})
}

// Close releases the journal file.
func (j *Journal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.f == nil {
		return nil
	}
	err := j.f.Close()
	j.f = nil
	return err
}

// Reset removes the journal after a fully successful run.
func (j *Journal) Reset() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.f != nil {
		_ = j.f.Close()
		j.f = nil
	}
	if err := os.Remove(j.path); err != nil && !os.IsNotExist(err) {
		return model.Wrap(model.KindState, "txn", "journal", j.path, err)
	}
	return nil
}

// Replay summarizes which segment/stage pairs reached a complete
// (begin+done) boundary. A begin without a matching done means the
// stage was interrupted and must be redone.
type Replay struct {
	// Done maps "stage/segmentID" to the closing entry.
	Done map[string]Entry
	// Pending lists stages that started but never completed.
	Pending []Entry
}

// Analyze folds journal entries into a replay summary.
func Analyze(entries []Entry) Replay {
	r := Replay{Done: map[string]Entry{}}
	begun := map[string]Entry{}
	for _, e := range entries {
		key := e.Stage + "/" + e.SegmentID
		switch e.State {
		case "begin":
			begun[key] = e
		case "done":
			r.Done[key] = e
			delete(begun, key)
		}
	}
	for _, e := range begun {
		r.Pending = append(r.Pending, e)
	}
	return r
}

// IsDone reports whether a stage completed for a segment.
func (r Replay) IsDone(stage, segmentID string) bool {
	_, ok := r.Done[stage+"/"+segmentID]
	return ok
}

// CopyReader is a small helper retained for snapshot tooling.
func CopyReader(dst io.Writer, src io.Reader) error {
	_, err := io.Copy(dst, src)
	return err
}
