package checkpoint

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Reset removes all checkpoint artefacts so a fresh run can start.
func (s *Store) Reset() error {
	for _, p := range []string{s.StatePath(), s.StagingPath(), s.OutputPath()} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("checkpoint: reset %s: %w", p, err)
		}
	}
	return nil
}

// writeAtomic writes data to path via a synced temporary file and rename.
func (s *Store) writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp*")
	if err != nil {
		return fmt.Errorf("checkpoint: temp: %w", err)
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("checkpoint: write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("checkpoint: sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("checkpoint: close: %w", err)
	}
	if err := os.Chmod(name, 0o644); err != nil {
		return fmt.Errorf("checkpoint: chmod: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("checkpoint: rename: %w", err)
	}
	return syncDir(filepath.Dir(path))
}

// WriteOutput atomically replaces the final output file.
func (s *Store) WriteOutput(data []byte) error {
	return s.writeAtomic(s.OutputPath(), data)
}

// ReadOutput reads the final output file.
func (s *Store) ReadOutput() ([]byte, error) {
	return os.ReadFile(s.OutputPath())
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("checkpoint: open dir: %w", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("checkpoint: sync dir: %w", err)
	}
	return nil
}

// HashPrefix returns the hex SHA-256 of the first n bytes of file.
func HashPrefix(file string, n int64) (string, error) {
	if n < 0 {
		return "", fmt.Errorf("checkpoint: negative prefix %d", n)
	}
	f, err := os.Open(file)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.CopyN(h, f, n); err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
