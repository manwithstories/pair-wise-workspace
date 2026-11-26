package media

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Limits bounds local resource use. Zero values are replaced by defaults.
type Limits struct {
	MaxInputBytes int64 `json:"max_input_bytes"`
	MaxWorkBytes  int64 `json:"max_work_bytes"`
	MaxConcurrency int  `json:"max_concurrency"`
	MaxCacheEntries int `json:"max_cache_entries"`
}

func DefaultLimits() Limits {
	return Limits{
		MaxInputBytes:   2 << 30, // 2 GiB per delivered asset
		MaxWorkBytes:    20 << 30,
		MaxConcurrency:  2,
		MaxCacheEntries: 256,
	}
}

func (l Limits) Normalize() Limits {
	d := DefaultLimits()
	if l.MaxInputBytes <= 0 {
		l.MaxInputBytes = d.MaxInputBytes
	}
	if l.MaxWorkBytes <= 0 {
		l.MaxWorkBytes = d.MaxWorkBytes
	}
	if l.MaxConcurrency <= 0 {
		l.MaxConcurrency = d.MaxConcurrency
	}
	if l.MaxCacheEntries <= 0 {
		l.MaxCacheEntries = d.MaxCacheEntries
	}
	if l.MaxConcurrency > 4 {
		l.MaxConcurrency = 4
	}
	return l
}

func (l Limits) CheckInputSize(size int64) error {
	if size < 0 {
		return NewError(CodeInvalidInput, "negative input size", nil)
	}
	if size > l.Normalize().MaxInputBytes {
		return NewError(CodeTooLarge, "input exceeds configured size limit", nil)
	}
	return nil
}

// SafeJoin confines name to root. It rejects absolute paths, NUL-like empty
// segments, and any lexical or evaluated symlink escape.
func SafeJoin(root, name string) (string, error) {
	if name == "" {
		return "", NewError(CodePathTraversal, "empty path component", nil)
	}
	if strings.ContainsRune(name, '\x00') {
		return "", NewError(CodePathTraversal, "NUL in path", nil)
	}
	if filepath.IsAbs(name) {
		return "", NewError(CodePathTraversal, "absolute artifact path is forbidden", nil)
	}
	cleanName := filepath.Clean(name)
	if cleanName == ".." || strings.HasPrefix(cleanName, ".."+string(filepath.Separator)) {
		return "", NewError(CodePathTraversal, "path traversal is forbidden", nil)
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", WrapError(CodeIO, "abs root", err)
	}
	joined := filepath.Join(absRoot, cleanName)
	rel, err := filepath.Rel(absRoot, joined)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", NewError(CodePathTraversal, "resolved path escapes root", err)
	}
	// If root exists, resolve symlinks before accepting an existing target.
	if realRoot, err := filepath.EvalSymlinks(absRoot); err == nil {
		if realTarget, err := filepath.EvalSymlinks(joined); err == nil {
			rel2, err := filepath.Rel(realRoot, realTarget)
			if err != nil || rel2 == ".." || strings.HasPrefix(rel2, ".."+string(filepath.Separator)) {
				return "", NewError(CodePathTraversal, "symlink escapes root", nil)
			}
		}
	}
	return joined, nil
}

// AtomicWriteFile writes to a same-directory temp file, fsyncs it, renames it,
// and fsyncs the parent directory. Readers can never observe a partial product.
func AtomicWriteFile(path string, r io.Reader, mode os.FileMode) (int64, string, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, "", WrapError(CodeIO, "create parent", err)
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return 0, "", WrapError(CodeIO, "create temp", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), r)
	if err != nil {
		_ = tmp.Close()
		cleanup()
		return n, "", WrapError(CodeIO, "write temp", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return n, "", WrapError(CodeIO, "fsync temp", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return n, "", WrapError(CodeIO, "close temp", err)
	}
	if mode != 0 {
		_ = os.Chmod(tmpName, mode)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return n, "", WrapError(CodeIO, "atomic rename", err)
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}

func FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", WrapError(CodeIO, "open for sha256", err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", WrapError(CodeIO, "hash file", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
