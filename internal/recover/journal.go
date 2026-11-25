// Package recover implements the deterministic stage journal used for
// reviewable recovery and atomic output replacement.
package recover

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"fieldcut/internal/clock"
)

const JournalName = "journal.json"

// Status values for a journal stage.
const (
	StatusCommitted = "committed"
	StatusFailed    = "failed"
)

// Record is one persisted stage transition.
type Record struct {
	Name        string            `json:"name"`
	Status      string            `json:"status"`
	InputSHA256 string            `json:"input_sha256"`
	Outputs     map[string]string `json:"output_sha256,omitempty"`
	Error       *clock.Error      `json:"error,omitempty"`
}

// Journal is the on-disk recovery log.
type Journal struct {
	Path    string   `json:"-"`
	Version int      `json:"version"`
	Stages  []Record `json:"stages"`
}

// Open reads a journal or returns an empty in-memory journal.
func Open(dir string) (*Journal, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, JournalName)
	j := &Journal{Path: path, Version: 1}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return j, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, j); err != nil {
		return nil, &clock.Error{Code: clock.CodeWrite, TrackID: -1, Wrapping: err, Message: "recovery journal is corrupt"}
	}
	j.Path = path
	if j.Version == 0 {
		j.Version = 1
	}
	return j, nil
}

func (j *Journal) index(name string) int {
	for i := range j.Stages {
		if j.Stages[i].Name == name {
			return i
		}
	}
	return -1
}

// Record returns a copy of the named stage record.
func (j *Journal) Record(name string) (Record, bool) {
	i := j.index(name)
	if i < 0 {
		return Record{}, false
	}
	return j.Stages[i], true
}

// IsCommitted reports whether name has a committed record for inputHash.
func (j *Journal) IsCommitted(name, inputHash string) bool {
	r, ok := j.Record(name)
	return ok && r.Status == StatusCommitted && r.InputSHA256 == inputHash
}

// ReusedNodes returns committed stage names matching input hashes.
func (j *Journal) ReusedNodes(currentHashes map[string]string) []string {
	out := make([]string, 0)
	for _, r := range j.Stages {
		if r.Status == StatusCommitted {
			if h, ok := currentHashes[r.Name]; ok && h == r.InputSHA256 {
				out = append(out, r.Name)
			}
		}
	}
	sort.Strings(out)
	return out
}

// Commit atomically persists a committed stage.
func (j *Journal) Commit(name, inputHash string, outputs map[string]string) error {
	r := Record{Name: name, Status: StatusCommitted, InputSHA256: inputHash, Outputs: outputs}
	if i := j.index(name); i >= 0 {
		j.Stages[i] = r
	} else {
		j.Stages = append(j.Stages, r)
	}
	return j.save()
}

// Fail persists a stage failure, including structured track/interval data.
func (j *Journal) Fail(name, inputHash string, runErr error) error {
	var ce *clock.Error
	if !errors.As(runErr, &ce) {
		ce = &clock.Error{Code: clock.CodeWrite, TrackID: -1, Message: runErr.Error(), Wrapping: runErr}
	}
	r := Record{Name: name, Status: StatusFailed, InputSHA256: inputHash, Error: ce}
	if i := j.index(name); i >= 0 {
		j.Stages[i] = r
	} else {
		j.Stages = append(j.Stages, r)
	}
	return j.save()
}

func (j *Journal) save() error {
	sort.SliceStable(j.Stages, func(i, k int) bool { return j.Stages[i].Name < j.Stages[k].Name })
	b, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return AtomicWriteFile(j.Path, b)
}

// AtomicWriteFile writes to a same-directory temporary file, fsyncs it and
// renames it over path. Readers therefore observe either the old or new file.
func AtomicWriteFile(path string, data []byte) error {
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
	if err := os.Chmod(tmpName, 0o644); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return err
	}
	return nil
}
