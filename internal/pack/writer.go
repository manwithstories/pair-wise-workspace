// Package pack builds the MPD model from an aligned ladder and writes
// outputs atomically. Rewriting an existing, differing output requires an
// explicit force flag; identical content is left untouched so repeated
// packs are byte-stable no-ops.
package pack

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"dashpack/internal/errs"
)

// WriteAtomic writes data to path via a same-directory temp file + rename.
// It reports whether the file was (re)written. Existing identical content
// is kept; differing content requires force.
func WriteAtomic(path string, data []byte, force bool) (bool, error) {
	existing, err := os.ReadFile(path)
	switch {
	case err == nil:
		if bytes.Equal(existing, data) {
			return false, nil
		}
		if !force {
			return false, errs.New(errs.ExitParam, errs.CodeOutputExists,
				fmt.Sprintf("output exists and differs: %s (use --force)", path),
				map[string]any{"path": path})
		}
	case errors.Is(err, fs.ErrNotExist):
		// fresh write
	default:
		return false, fmt.Errorf("pack: read %s: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, fmt.Errorf("pack: mkdir: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".dashpack-*")
	if err != nil {
		return false, fmt.Errorf("pack: temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return false, fmt.Errorf("pack: write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return false, fmt.Errorf("pack: close temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return false, fmt.Errorf("pack: rename: %w", err)
	}
	return true, nil
}
