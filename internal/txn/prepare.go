package txn

import (
	"os"
	"path/filepath"
)

// Prepare clears the artifacts this pipeline owns so a fresh run starts from
// a known state. Only names created by this package are removed; anything
// else in dir is left alone.
func Prepare(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	managed := map[string]bool{
		"manifest.json":  true,
		"wal.jsonl":      true,
		"transport.json": true,
		"corrupt.json":   true,
		"manifest.jsonl": true,
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		isSegment := len(name) > 8 && name[:4] == "seg-" && filepath.Ext(name) == ".json"
		if !managed[name] && !isSegment {
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	for _, sub := range []string{"frames", "spill"} {
		if err := os.RemoveAll(filepath.Join(dir, sub)); err != nil {
			return err
		}
	}
	return nil
}
