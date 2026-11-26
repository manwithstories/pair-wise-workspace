package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"pairflow/internal/media"
)

// Record is one durable stage entry. A record is appended and fsynced before
// the stage's artifacts are written, so a crash between the two leaves a
// "pending" record that resume can safely repeat.
type Record struct {
	Stage    string `json:"stage"`
	State    string `json:"state"` // begin | done | failed
	Seq      int    `json:"seq"`
	Timeline string `json:"timeline_sha256,omitempty"`
	// Artifacts accepted at "done".
	Artifacts []Artifact `json:"artifacts,omitempty"`
	Error     string     `json:"error,omitempty"`
	At        string     `json:"at"`
}

// Artifact pins one stage output by re-readable path, size and hash.
type Artifact struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
	SHA  string `json:"sha256"`
	Kind string `json:"kind"`
}

// WAL is an append-only, fsynced stage journal at <run>/wal.jsonl.
type WAL struct {
	path string
	f    *os.File
	seq  int
	recs []Record
}

const walName = "wal.jsonl"

// OpenWAL creates or reopens the journal. Existing records are loaded so a
// resumed run continues the same sequence.
func OpenWAL(root string) (*WAL, *media.Error) {
	path := filepath.Join(root, walName)
	w := &WAL{path: path}
	if data, err := os.ReadFile(path); err == nil {
		if err := w.load(data); err != nil {
			return nil, err
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, media.Wrap(err, media.ClassInternal, "wal.open", "cannot open write-ahead log")
	}
	w.f = f
	return w, nil
}

func (w *WAL) load(data []byte) *media.Error {
	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		if line == "" {
			continue
		}
		var r Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			return media.Wrap(err, media.ClassIntegrity, "wal.corrupt",
				"journal line %d is not valid JSON", i+1).WithPath(w.path)
		}
		w.recs = append(w.recs, r)
		if r.Seq >= w.seq {
			w.seq = r.Seq + 1
		}
	}
	return nil
}

// Records returns the journal entries loaded so far.
func (w *WAL) Records() []Record { return append([]Record(nil), w.recs...) }

// Append writes and fsyncs one record. The fsync happens before the call
// returns, which is the durability point the pipeline relies on.
func (w *WAL) Append(r Record) *media.Error {
	if w.f == nil {
		return media.E(media.ClassInternal, "wal.closed", "journal is closed")
	}
	r.Seq = w.seq
	r.At = time.Now().UTC().Format(time.RFC3339Nano)
	data, err := json.Marshal(r)
	if err != nil {
		return media.Wrap(err, media.ClassInternal, "wal.encode", "cannot encode journal record")
	}
	if _, err := w.f.Write(append(data, '\n')); err != nil {
		return media.Wrap(err, media.ClassInternal, "wal.write", "cannot write journal record")
	}
	if err := w.f.Sync(); err != nil {
		return media.Wrap(err, media.ClassInternal, "wal.sync", "cannot fsync journal")
	}
	w.seq++
	w.recs = append(w.recs, r)
	return nil
}

// Begin logs a stage start.
func (w *WAL) Begin(stage, timeline string) *media.Error {
	return w.Append(Record{Stage: stage, State: "begin", Timeline: timeline})
}

// Done logs a stage completion with its accepted artifacts.
func (w *WAL) Done(stage, timeline string, arts []Artifact) *media.Error {
	return w.Append(Record{Stage: stage, State: "done", Timeline: timeline, Artifacts: arts})
}

// Fail logs a stage failure before any rollback runs.
func (w *WAL) Fail(stage, timeline, msg string) *media.Error {
	return w.Append(Record{Stage: stage, State: "failed", Timeline: timeline, Error: msg})
}

// LastDone returns the most recent successfully committed stage for timeline.
func (w *WAL) LastDone(timeline string) (Record, bool) {
	for i := len(w.recs) - 1; i >= 0; i-- {
		r := w.recs[i]
		if r.State == "done" && (timeline == "" || r.Timeline == timeline) {
			return r, true
		}
	}
	return Record{}, false
}

// DoneStages lists committed stage names in order.
func (w *WAL) DoneStages(timeline string) []string {
	var out []string
	for _, r := range w.recs {
		if r.State == "done" && (timeline == "" || r.Timeline == timeline) {
			out = append(out, r.Stage)
		}
	}
	return out
}

// VerifyArtifacts re-reads every artifact recorded for a stage and confirms
// its size and hash still match. This is what makes resume trust only
// validated stages.
func (j *Jail) VerifyArtifacts(arts []Artifact) *media.Error {
	for _, a := range arts {
		hash, size, err := j.HashFile(a.Path)
		if err != nil {
			return err
		}
		if size != a.Size {
			return media.Integrity("store.size_mismatch",
				"artifact %s is %d bytes, journal recorded %d", a.Path, size, a.Size).WithPath(a.Path)
		}
		if hash != a.SHA {
			return media.Integrity("store.hash_mismatch",
				"artifact %s hash %s does not match journal %s", a.Path, hash, a.SHA).WithPath(a.Path)
		}
	}
	return nil
}

// SyncDir fsyncs the directory holding an artifact so a rename is durable.
func SyncDir(dir string) *media.Error {
	f, err := os.Open(dir)
	if err != nil {
		return media.Wrap(err, media.ClassInternal, "store.dir_open", "cannot open directory for sync")
	}
	defer f.Close()
	if err := f.Sync(); err != nil {
		return media.Wrap(err, media.ClassInternal, "store.dir_sync", "cannot fsync directory")
	}
	return nil
}

// Close releases the journal handle.
func (w *WAL) Close() error {
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}

// String renders a record compactly for terminal output.
func (r Record) String() string {
	return fmt.Sprintf("%s:%s#%d", r.Stage, r.State, r.Seq)
}
