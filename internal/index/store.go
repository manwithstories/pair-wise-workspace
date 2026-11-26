package index

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// IndexFileName is the state-file name for the persisted index.
const IndexFileName = "index.json"

// AtomicWriteJSON writes v as deterministic JSON to path, atomically
// (write to a temp file in the same directory, then rename).
func AtomicWriteJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LoadOrBuild returns the stored index when its input and config
// digests still match; otherwise it rebuilds the index and persists it
// atomically. The boolean result reports whether a rebuild happened
// (i.e. the previous index, if any, was invalidated).
func LoadOrBuild(stateDir, root string, cfg Config) (*Index, bool, error) {
	digest, err := HashInput(root)
	if err != nil {
		return nil, false, err
	}
	path := filepath.Join(stateDir, IndexFileName)
	if data, err := os.ReadFile(path); err == nil {
		var idx Index
		if err := json.Unmarshal(data, &idx); err == nil &&
			idx.InputDigest == digest && idx.ConfigDigest == cfg.Digest() {
			return &idx, false, nil
		}
	}
	idx, err := Build(root, cfg)
	if err != nil {
		return nil, false, err
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, false, err
	}
	if err := AtomicWriteJSON(path, idx); err != nil {
		return nil, false, err
	}
	return idx, true, nil
}
