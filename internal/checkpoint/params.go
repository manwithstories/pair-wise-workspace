package checkpoint

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrUnreadable is returned when a persisted document cannot be read because
// its path is missing or permission is denied (an input fault, not corruption).
var ErrUnreadable = errors.New("checkpoint: persisted document unreadable")

// paramsName stores the full run parameters so an independent verifier can
// reconstruct the digest without the original command line.
const paramsName = "params.json"

// SaveParams writes the parameter document once when a run directory opens.
func SaveParams(dir string, p Params) error {
	path := filepath.Join(dir, paramsName)
	if _, err := os.Stat(path); err == nil {
		return nil // idempotent: never overwrite an existing document
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return fmt.Errorf("%w: %v", ErrCheckpointCorrupt, err)
	}
	return os.WriteFile(path, b, 0o644)
}

// LoadParams reads the parameter document written by SaveParams.
func LoadParams(dir string) (Params, error) {
	var p Params
	b, err := os.ReadFile(filepath.Join(dir, paramsName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
			return p, fmt.Errorf("%w: %v", ErrUnreadable, err)
		}
		return p, fmt.Errorf("%w: %v", ErrCheckpointCorrupt, err)
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return p, fmt.Errorf("%w: %v", ErrCheckpointCorrupt, err)
	}
	return p, nil
}
