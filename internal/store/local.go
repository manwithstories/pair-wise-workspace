// Package store implements bounded, atomic local segment persistence.
package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/pairwise/mediad/internal/media"
)

const (
	// MaxSegmentBytes is the hard 64 MiB per-segment limit.
	MaxSegmentBytes = 64 << 20
	// MaxTasks is the global concurrency bound for local pipeline tasks.
	MaxTasks = 4
)

// LocalStore root-confines all reads and writes.
type LocalStore struct {
	Root string
}

// New creates root if needed and returns a confined store.
func New(root string) (*LocalStore, error) {
	if strings.TrimSpace(root) == "" {
		return nil, media.NewError(media.CodeInvalidArgument, "store.local", "root", "empty output directory")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, media.Wrap(err, media.CodeLocalIO, "store.local", "root", "cannot resolve output directory")
	}
	if err := os.MkdirAll(abs, 0o750); err != nil {
		return nil, media.Wrap(err, media.CodeLocalIO, "store.local", "root", "cannot create output directory")
	}
	return &LocalStore{Root: abs}, nil
}

// SafePath resolves rel inside Root and rejects absolute paths and traversal.
func (s *LocalStore) SafePath(rel string) (string, error) {
	if rel == "" || filepath.IsAbs(rel) || strings.Contains(rel, "\x00") {
		return "", media.NewError(media.CodePathViolation, "store.local", "path", "absolute or empty path rejected")
	}
	clean := filepath.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", media.NewError(media.CodePathViolation, "store.local", "path", "path traversal rejected: "+rel)
	}
	full := filepath.Join(s.Root, clean)
	relToRoot, err := filepath.Rel(s.Root, full)
	if err != nil || relToRoot == ".." || strings.HasPrefix(relToRoot, ".."+string(filepath.Separator)) {
		return "", media.NewError(media.CodePathViolation, "store.local", "path", "path escapes store root: "+rel)
	}
	return full, nil
}

// WriteAtomic writes data via a same-directory temp file and rename. It rejects
// segments larger than 64 MiB before creating the temp file.
func (s *LocalStore) WriteAtomic(rel string, data []byte) error {
	if len(data) > MaxSegmentBytes {
		return media.NewError(media.CodeTooLarge, "store.local", rel, "segment exceeds 64 MiB")
	}
	full, err := s.SafePath(rel)
	if err != nil {
		return err
	}
	dir := filepath.Dir(full)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return media.Wrap(err, media.CodeLocalIO, "store.local", rel, "cannot create parent directory")
	}
	tmp, err := os.CreateTemp(dir, ".tmp-segment-*")
	if err != nil {
		return media.Wrap(err, media.CodeLocalIO, "store.local", rel, "cannot create temporary file")
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return media.Wrap(err, media.CodeLocalIO, "store.local", rel, "atomic write failed")
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return media.Wrap(err, media.CodeLocalIO, "store.local", rel, "fsync failed")
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return media.Wrap(err, media.CodeLocalIO, "store.local", rel, "close failed")
	}
	if err := os.Chmod(tmpName, 0o640); err != nil {
		cleanup()
		return media.Wrap(err, media.CodeLocalIO, "store.local", rel, "chmod failed")
	}
	if err := os.Rename(tmpName, full); err != nil {
		cleanup()
		return media.Wrap(err, media.CodeLocalIO, "store.local", rel, "rename failed")
	}
	return nil
}

// Read reads a root-confined file.
func (s *LocalStore) Read(rel string) ([]byte, error) {
	full, err := s.SafePath(rel)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(full)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, media.Wrap(err, media.CodeNotFound, "store.local", rel, "segment not found")
		}
		return nil, media.Wrap(err, media.CodeLocalIO, "store.local", rel, "read failed")
	}
	if len(data) > MaxSegmentBytes {
		return nil, media.NewError(media.CodeTooLarge, "store.local", rel, "stored segment exceeds 64 MiB")
	}
	return data, nil
}

// Exists reports whether rel exists and is a regular file.
func (s *LocalStore) Exists(rel string) bool {
	full, err := s.SafePath(rel)
	if err != nil {
		return false
	}
	st, err := os.Stat(full)
	return err == nil && !st.IsDir()
}

// Semaphore is a small fixed-capacity task limiter.
type Semaphore chan struct{}

// NewSemaphore creates a concurrency limiter; values above MaxTasks clamp to 4.
func NewSemaphore(n int) Semaphore {
	if n <= 0 || n > MaxTasks {
		n = MaxTasks
	}
	return make(Semaphore, n)
}

// Run executes fn while holding one slot.
func (sem Semaphore) Run(fn func() error) error {
	sem <- struct{}{}
	defer func() { <-sem }()
	return fn()
}
