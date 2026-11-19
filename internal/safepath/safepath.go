// Package safepath validates output paths and writes files atomically.
// Every module that writes to disk goes through it so path traversal
// and partial writes are handled in one place.
package safepath

import (
	"os"
	"path/filepath"
	"strings"

	"pairflow/media/internal/clock"
)

// Resolve cleans dir, rejects escape attempts and returns an absolute
// path.
func Resolve(dir string) (string, error) {
	if strings.TrimSpace(dir) == "" {
		return "", clock.NewSourceError("safepath", "bad_output_path", -1, "empty output directory")
	}
	clean := filepath.Clean(dir)
	for _, part := range strings.Split(filepath.ToSlash(clean), "/") {
		if part == ".." {
			return "", clock.Errorf("safepath", "path_traversal", -1,
				"output path %q escapes the working tree", dir)
		}
	}
	abs, err := filepath.Abs(clean)
	if err != nil {
		return "", err
	}
	if abs == string(os.PathSeparator) {
		return "", clock.NewSourceError("safepath", "bad_output_path", -1,
			"refusing to write to the filesystem root")
	}
	return abs, nil
}

// EnsureDir resolves dir and creates it.
func EnsureDir(dir string) (string, error) {
	root, err := Resolve(dir)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	return root, nil
}

// WriteAtomic writes data to path via a temporary file in root followed
// by a rename. path must live inside root.
func WriteAtomic(path string, data []byte, root string) error {
	if !strings.HasPrefix(path, root+string(os.PathSeparator)) {
		return clock.Errorf("safepath", "path_escape", -1,
			"refusing to write %q outside %q", path, root)
	}
	tmp, err := os.CreateTemp(root, ".pairflow-*.tmp")
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
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// Join builds a path under root after checking that the joined result
// stays inside root. Relative names containing separators are allowed
// as long as they do not traverse upward.
func Join(root, name string) (string, error) {
	if name == "" || filepath.IsAbs(name) {
		return "", clock.Errorf("safepath", "bad_output_path", -1, "invalid file name %q", name)
	}
	joined := filepath.Join(root, name)
	if joined != root && !strings.HasPrefix(joined, root+string(os.PathSeparator)) {
		return "", clock.Errorf("safepath", "path_escape", -1,
			"refusing to write %q outside %q", name, root)
	}
	return joined, nil
}
