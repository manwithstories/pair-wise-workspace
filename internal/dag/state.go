package dag

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"review/internal/media"
)

// IdempotencyKey hashes sorted input references and the parameter blob.
// Identical (inputs, params) always map to the same key.
func IdempotencyKey(inputs map[string]string, params any) (string, error) {
	keys := make([]string, 0, len(inputs))
	for k := range inputs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		fmt.Fprintf(h, "%s=%s\n", k, inputs[k])
	}
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			return "", media.NewInternal("dag-runner", "marshal params", err)
		}
		h.Write(raw)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// load reads the durable state file if present and reconstructs replayable
// failure errors so a resumed runner preserves class/dependency provenance.
func (rt *Runner) load() error {
	raw, err := os.ReadFile(rt.statePath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return media.NewInternal("dag-runner", "read state", err)
	}
	if err := json.Unmarshal(raw, &rt.state); err != nil {
		return media.NewInternal("dag-runner", "parse state", err)
	}
	for name, st := range rt.state {
		if st.Status != StatusFailed {
			continue
		}
		reason := st.Reason
		if reason == "" {
			reason = st.Error
		}
		blockOff := st.BlockOff
		if blockOff == 0 {
			blockOff = -1
		}
		rt.lastError[name] = &media.PipelineError{
			Node: name, Class: st.Class, Track: st.Track,
			BlockOff: blockOff, Dependency: st.Dependency, Reason: reason,
		}
	}
	return nil
}

// persist writes state to a temp file and atomically renames it.
func (rt *Runner) persist() error {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.persistLocked()
}

// persistLocked performs the atomic write assuming the mutex is held.
func (rt *Runner) persistLocked() error {
	raw, err := json.MarshalIndent(rt.state, "", "  ")
	if err != nil {
		return media.NewInternal("dag-runner", "marshal state", err)
	}
	dir := filepath.Dir(rt.statePath)
	tmp, err := os.CreateTemp(dir, ".dag-*.tmp")
	if err != nil {
		return media.NewInternal("dag-runner", "temp state", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return media.NewInternal("dag-runner", "write state", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return media.NewInternal("dag-runner", "sync state", err)
	}
	if err := tmp.Close(); err != nil {
		return media.NewInternal("dag-runner", "close state", err)
	}
	if err := os.Rename(tmpName, rt.statePath); err != nil {
		return media.NewInternal("dag-runner", "commit state", err)
	}
	return nil
}
