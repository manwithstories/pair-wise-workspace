package scheduler

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

const stateFile = "state.json"

// SaveState persists the runner state atomically into dir.
func (r *Runner) SaveState(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(r.state, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, stateFile+".tmp")
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, stateFile))
}

// LoadState restores a runner from dir. The paramsHash must match the
// stored run; otherwise resuming would silently mix parameters.
func LoadState(dir, paramsHash string) (*Runner, error) {
	raw, err := os.ReadFile(filepath.Join(dir, stateFile))
	if err != nil {
		return nil, err
	}
	var st State
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("scheduler: corrupt state file: %w", err)
	}
	if st.ParamsHash != paramsHash {
		return nil, fmt.Errorf("scheduler: params changed since checkpoint, cannot resume")
	}
	r := New(paramsHash)
	r.state = &st
	if r.state.Nodes == nil {
		r.state.Nodes = map[string]*NodeRecord{}
	}
	return r, nil
}

// StateExists reports whether a resumable checkpoint exists in dir.
func StateExists(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, stateFile))
	return err == nil
}
