// Package checkpoint persists pipeline state atomically so an
// interrupted run can resume from the last complete boundary.
package checkpoint

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrDigestMismatch rejects reuse when the input or configuration
// changed since the checkpoint was written.
var ErrDigestMismatch = errors.New("checkpoint digest mismatch")

// Checkpoint captures everything needed to resume a run.
type Checkpoint struct {
	Version          int             `json:"version"`
	InputSHA256      string          `json:"input_sha256"`
	ConfigSHA256     string          `json:"config_sha256"`
	RecordsProcessed int             `json:"records_processed"`
	OutputCommitted  int64           `json:"output_committed"`
	Pipeline         json.RawMessage `json:"pipeline"`
}

// Save writes the checkpoint via a temporary file and atomic rename.
func Save(path string, cp *Checkpoint) error {
	cp.Version = 1
	data, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Load reads a checkpoint; a missing file is not an error.
func Load(path string) (*Checkpoint, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var cp Checkpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return nil, fmt.Errorf("checkpoint corrupt: %w", err)
	}
	if cp.Version != 1 {
		return nil, fmt.Errorf("checkpoint corrupt: version %d", cp.Version)
	}
	return &cp, nil
}

// Validate refuses reuse when input or config digests differ.
func (cp *Checkpoint) Validate(inputSHA, configSHA string) error {
	if cp.InputSHA256 != inputSHA {
		return fmt.Errorf("%w: input changed (%s != %s)",
			ErrDigestMismatch, cp.InputSHA256, inputSHA)
	}
	if cp.ConfigSHA256 != configSHA {
		return fmt.Errorf("%w: config changed", ErrDigestMismatch)
	}
	return nil
}

// CorruptFile is a test/helper hook that truncates a checkpoint file.
func CorruptFile(path string) error {
	p := filepath.Clean(path)
	return os.WriteFile(p, []byte("{bad json"), 0o600)
}
