package checkpoint

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"mediapipe/internal/media"
)

type Artifact struct {
	Name        string    `json:"name"`
	Path        string    `json:"path"`
	SHA256      string    `json:"sha256"`
	SizeBytes   int64     `json:"size_bytes"`
	CompletedAt time.Time `json:"completed_at"`
}

type State struct {
	JobID     string              `json:"job_id"`
	SourceSHA string              `json:"source_sha256"`
	ConfigSHA string              `json:"config_sha256"`
	NextSeq   uint64              `json:"next_seq,omitempty"`
	Artifacts map[string]Artifact `json:"artifacts"`
	UpdatedAt time.Time           `json:"updated_at"`
}

type Journal struct {
	path  string
	mu    sync.Mutex
	state State
}

// Open loads an existing journal. A torn/partial journal should be impossible
// because all updates are atomic; malformed content is reported as code 4.
func Open(path, jobID, sourceSHA, configSHA string) (*Journal, error) {
	j := &Journal{path: path, state: State{
		JobID: jobID, SourceSHA: sourceSHA, ConfigSHA: configSHA,
		Artifacts: map[string]Artifact{},
	}}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return j, nil
	}
	if err != nil {
		return nil, media.WrapError(media.CodeCheckpoint, "read journal", err)
	}
	if err := json.Unmarshal(b, &j.state); err != nil {
		return nil, media.WrapError(media.CodeCheckpoint, "parse journal", err)
	}
	if j.state.Artifacts == nil {
		j.state.Artifacts = map[string]Artifact{}
	}
	// A journal from a different source/config cannot safely resume this job.
	if sourceSHA != "" && j.state.SourceSHA != "" && j.state.SourceSHA != sourceSHA {
		return nil, media.NewError(media.CodeCheckpoint, "journal source digest does not match job", nil)
	}
	if configSHA != "" && j.state.ConfigSHA != "" && j.state.ConfigSHA != configSHA {
		return nil, media.NewError(media.CodeCheckpoint, "journal encoding config does not match job", nil)
	}
	j.state.JobID = jobID
	j.state.SourceSHA = sourceSHA
	j.state.ConfigSHA = configSHA
	return j, nil
}

func (j *Journal) Snapshot() State {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := j.state
	out.Artifacts = make(map[string]Artifact, len(j.state.Artifacts))
	for k, v := range j.state.Artifacts {
		out.Artifacts[k] = v
	}
	return out
}

func (j *Journal) IsComplete(name string) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	_, ok := j.state.Artifacts[name]
	return ok
}

func (j *Journal) SetNextSequence(seq uint64) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.state.NextSeq = seq
	return j.saveLocked()
}

// RecordProduct hashes and stats a product that has already been atomically
// renamed into place, then atomically advances the journal.
func (j *Journal) RecordProduct(name, path string, nextSeq uint64) (Artifact, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	st, err := os.Stat(path)
	if err != nil {
		return Artifact{}, media.WrapError(media.CodeIO, "stat completed product", err)
	}
	if st.IsDir() {
		return Artifact{}, media.NewError(media.CodeIO, "product path is a directory", nil)
	}
	sum, err := media.FileSHA256(path)
	if err != nil {
		return Artifact{}, err
	}
	if old, ok := j.state.Artifacts[name]; ok && old.SHA256 != sum {
		return Artifact{}, media.NewError(media.CodeCheckpoint, "completed artifact digest changed", nil)
	}
	a := Artifact{Name: name, Path: path, SHA256: sum, SizeBytes: st.Size(), CompletedAt: time.Now().UTC()}
	j.state.Artifacts[name] = a
	if nextSeq != 0 {
		j.state.NextSeq = nextSeq
	}
	if err := j.saveLocked(); err != nil {
		return Artifact{}, err
	}
	return a, nil
}

func (j *Journal) saveLocked() error {
	j.state.UpdatedAt = time.Now().UTC()
	b, err := json.MarshalIndent(j.state, "", "  ")
	if err != nil {
		return media.WrapError(media.CodeCheckpoint, "marshal journal", err)
	}
	if err := os.MkdirAll(filepath.Dir(j.path), 0o755); err != nil {
		return media.WrapError(media.CodeCheckpoint, "create journal directory", err)
	}
	_, _, err = media.AtomicWriteFile(j.path, bytes.NewReader(b), 0o644)
	if err != nil {
		return media.WrapError(media.CodeCheckpoint, "atomic journal write", err)
	}
	return nil
}
