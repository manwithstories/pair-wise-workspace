// Package cache provides a content-addressed blob store and atomic,
// chain-verified checkpoints so interrupted runs can resume reproducibly.
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"avcheck/internal/fault"
)

// Store is a content-addressed blob store rooted at Dir.
type Store struct{ Dir string }

// DigestOf returns the sha256 hex of data.
func DigestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Put stores data under its digest and returns the digest. The write is
// atomic: a temp file in the same directory is renamed into place, so an
// interrupted run never leaves a partial blob.
func (s *Store) Put(data []byte) (string, error) {
	id := DigestOf(data)
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return "", err
	}
	final := filepath.Join(s.Dir, id)
	if _, err := os.Stat(final); err == nil {
		return id, nil // already stored
	}
	tmp, err := os.CreateTemp(s.Dir, ".tmp-*")
	if err != nil {
		return "", err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	if err := os.Rename(tmp.Name(), final); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return id, nil
}

// Get reads a blob and re-verifies its digest; a corrupted blob is reported
// as recoverable corruption, not returned.
func (s *Store) Get(id string) ([]byte, error) {
	data, err := os.ReadFile(filepath.Join(s.Dir, id))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fault.New(fault.Corrupt, "cache.get", 0, "missing blob "+id)
		}
		return nil, err
	}
	if DigestOf(data) != id {
		return nil, fault.New(fault.Corrupt, "cache.get", 0, "blob digest mismatch "+id)
	}
	return data, nil
}

// Has reports whether a verified blob exists.
func (s *Store) Has(id string) bool {
	_, err := s.Get(id)
	return err == nil
}
