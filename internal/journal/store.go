package journal

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Version identifies the on-disk checkpoint schema.
const Version = 1

// ErrCorruptCheckpoint marks an unreadable or inconsistent checkpoint.
var ErrCorruptCheckpoint = errors.New("journal: corrupt checkpoint")

// Record is one append-only trace line. It is never part of the output
// hash; timestamps here do not affect reproducibility.
type Record struct {
	Version      int       `json:"version"`
	Seq          int       `json:"seq"`
	Stage        string    `json:"stage"`
	InputDigest  string    `json:"input_digest"`
	Source       string    `json:"source,omitempty"`
	Track        string    `json:"track,omitempty"`
	Segment      string    `json:"segment,omitempty"`
	BeforePTS    string    `json:"before_pts,omitempty"`
	AfterPTS     string    `json:"after_pts,omitempty"`
	SourceOffset string    `json:"source_offset,omitempty"`
	Detail       string    `json:"detail,omitempty"`
	OutputSHA256 string    `json:"output_sha256,omitempty"`
	ResultSHA256 string    `json:"result_sha256,omitempty"`
	Time         time.Time `json:"time"`
}

// Checkpoint captures completed stages and their artifact hashes.
type Checkpoint struct {
	Version     int               `json:"version"`
	InputDigest string            `json:"input_digest"`
	Completed   []string          `json:"completed"`
	Hashes      map[string]string `json:"hashes"`
	Artifacts   map[string]string `json:"artifacts"`
	ResultSHA   string            `json:"result_sha256,omitempty"`
	Finalized   bool              `json:"finalized"`
}

// Store is a state directory holding checkpoint.json, journal.jsonl and
// per-stage artifacts.
type Store struct {
	Dir string
}

// Open creates the state directory tree.
func Open(dir string) (*Store, error) {
	if dir == "" {
		return nil, fmt.Errorf("journal: empty state directory")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Store{Dir: dir}, nil
}

// Path returns a path inside the state directory.
func (s *Store) Path(name string) string { return filepath.Join(s.Dir, name) }

// AtomicWrite writes data to a temporary file, verifies the bytes on
// disk, fsyncs and renames it into place. Any partial file is removed.
func AtomicWrite(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() {
		tmp.Close()
		os.Remove(tmpName)
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	got, err := os.ReadFile(tmpName)
	if err != nil || sha256Hex(got) != sha256Hex(data) {
		os.Remove(tmpName)
		if err == nil {
			err = fmt.Errorf("journal: verification mismatch for %s", path)
		}
		return err
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// SaveCheckpoint atomically writes the checkpoint.
func (s *Store) SaveCheckpoint(cp Checkpoint) error {
	cp.Version = Version
	if cp.Hashes == nil {
		cp.Hashes = map[string]string{}
	}
	if cp.Artifacts == nil {
		cp.Artifacts = map[string]string{}
	}
	data, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return err
	}
	return AtomicWrite(filepath.Join(s.Dir, "checkpoint.json"), append(data, '\n'), 0o644)
}

// LoadCheckpoint returns the checkpoint, or (nil, nil) when none exists.
func (s *Store) LoadCheckpoint() (*Checkpoint, error) {
	data, err := os.ReadFile(filepath.Join(s.Dir, "checkpoint.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var cp Checkpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorruptCheckpoint, err)
	}
	if cp.Version != Version {
		return nil, fmt.Errorf("%w: version %d", ErrCorruptCheckpoint, cp.Version)
	}
	if cp.Hashes == nil {
		cp.Hashes = map[string]string{}
	}
	if cp.Artifacts == nil {
		cp.Artifacts = map[string]string{}
	}
	return &cp, nil
}

// sha256Hex is shared with digest.go.
func sha256Hex(data []byte) string { return hexSum(data) }
