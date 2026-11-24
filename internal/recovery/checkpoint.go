// Package recovery persists deterministic processing checkpoints atomically.
package recovery

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/clearliu/mediq/internal/core"
)

// Checkpoint is the resumable committed position plus input/parameter binds.
type Checkpoint struct {
	Version                 int    `json:"version"`
	Case                    string `json:"case,omitempty"`
	InputSHA256             string `json:"input_sha256"`
	ParamsSHA256            string `json:"params_sha256"`
	CommittedOutputSamples  int64  `json:"committed_output_samples"`
	CommittedExtendedSeq    int64  `json:"committed_extended_seq"`
	ProcessedRecords        int    `json:"processed_records"`
	Completed               bool   `json:"completed"`
	ProductSHA256           string `json:"product_sha256,omitempty"`
}

// Store saves checkpoints at one path with injectable crash points.
type Store struct {
	Path string

	// BeforeRename is called after the temp file is synced and closed.
	BeforeRename func(cp Checkpoint) error
	// AfterRename is called after the atomic replacement and directory sync.
	AfterRename func(cp Checkpoint) error
}

// DefaultCheckpointName is the stable checkpoint filename.
const DefaultCheckpointName = "checkpoint.json"

// NewStore returns a store in outDir.
func NewStore(outDir string) *Store {
	return &Store{Path: filepath.Join(outDir, DefaultCheckpointName)}
}

// Load reads a checkpoint. A missing file returns nil without error.
func Load(path string) (*Checkpoint, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, core.New(core.KindIO, "read-checkpoint", err)
	}
	var cp Checkpoint
	if err := json.Unmarshal(b, &cp); err != nil {
		return nil, core.At(core.KindCheckpoint, "decode-checkpoint", 0, err)
	}
	if cp.Version != 1 {
		return nil, core.New(core.KindCheckpoint, "checkpoint-version",
			fmt.Errorf("unsupported version %d", cp.Version))
	}
	return &cp, nil
}

// Load calls Load at the store path.
func (s *Store) Load() (*Checkpoint, error) { return Load(s.Path) }

// Save atomically writes cp. A failure before rename leaves the previous file
// untouched.
func (s *Store) Save(cp Checkpoint) error {
	cp.Version = 1
	data, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return core.New(core.KindConfig, "encode-checkpoint", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(s.Path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return core.New(core.KindIO, "create-checkpoint-dir", err)
	}

	tmp, err := os.CreateTemp(dir, ".checkpoint-*.tmp")
	if err != nil {
		return core.New(core.KindIO, "create-temp-checkpoint", err)
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return core.New(core.KindIO, "write-temp-checkpoint", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return core.New(core.KindIO, "sync-temp-checkpoint", err)
	}
	if err := tmp.Close(); err != nil {
		return core.New(core.KindIO, "close-temp-checkpoint", err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return core.New(core.KindIO, "chmod-temp-checkpoint", err)
	}
	if s.BeforeRename != nil {
		if err := s.BeforeRename(cp); err != nil {
			return err
		}
	}
	if err := os.Rename(tmpName, s.Path); err != nil {
		return core.New(core.KindIO, "rename-checkpoint", err)
	}
	cleanup = false
	syncDir(dir)

	if s.AfterRename != nil {
		return s.AfterRename(cp)
	}
	return nil
}

// ValidateResume ensures the checkpoint binds the same input and parameters.
func (c *Checkpoint) ValidateResume(inputSHA, paramsSHA string) error {
	if c == nil {
		return nil
	}
	if c.InputSHA256 != inputSHA {
		return core.New(core.KindCheckpoint, "resume-input-mismatch",
			fmt.Errorf("checkpoint input %s != current %s", c.InputSHA256, inputSHA))
	}
	if c.ParamsSHA256 != paramsSHA {
		return core.New(core.KindConfig, "resume-params-mismatch",
			fmt.Errorf("checkpoint params %s != current %s", c.ParamsSHA256, paramsSHA))
	}
	return nil
}

// DigestJSON returns a stable SHA-256 digest of JSON-encodable parameters.
func DigestJSON(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", core.New(core.KindConfig, "digest-params", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// SHA256File returns the hex SHA-256 of a local file.
func SHA256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", core.New(core.KindIO, "open-for-hash", err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", core.New(core.KindIO, "hash-file", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}
