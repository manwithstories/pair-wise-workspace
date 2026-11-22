package cache

import (
	"os"
	"path/filepath"

	"mediapack/internal/fault"
)

// atomicWrite writes to a temp file in the same directory and renames into
// place, so readers never observe a partially written cache entry.
func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fault.Wrap(err, "cache")
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		cleanup()
		return fault.Wrap(err, "cache")
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		cleanup()
		return fault.Wrap(err, "cache")
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fault.Wrap(err, "cache")
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return fault.Wrap(err, "cache")
	}
	return nil
}
