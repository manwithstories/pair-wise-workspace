// Package index computes content digests of delivery packages and persists
// the scan index. The digest of (input files, normalizer config, parser
// version) is the cache key: any change forces a rebuild.
package index

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"aircheck/internal/fault"
)

// ParserVersion is bumped whenever scan/event parsing semantics change;
// it is part of the cache key so stale indexes are never reused.
const ParserVersion = 3

// MaxFileBytes bounds single input files; larger files are rejected
// rather than silently truncated.
const MaxFileBytes = 2 << 30 // 2 GiB

// FileEntry describes one hashed input file.
type FileEntry struct {
	Path string `json:"path"` // relative to input root
	Size int64  `json:"size"`
	SHA  string `json:"sha256"`
}

// DigestInput is everything that keys the cache.
type DigestInput struct {
	Files         []FileEntry `json:"files"`
	Config        string      `json:"config"` // canonical normalizer/plan config string
	ParserVersion int         `json:"parser_version"`
}

// HashFile streams a file through SHA-256 with a size cap.
func HashFile(path string) (FileEntry, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return FileEntry{}, err
	}
	if fi.Size() > MaxFileBytes {
		return FileEntry{}, fault.New(fault.CodeRuntime, filepath.Base(path), -1, false,
			"file exceeds %d byte limit: %s", MaxFileBytes, path)
	}
	f, err := os.Open(path)
	if err != nil {
		return FileEntry{}, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, io.LimitReader(f, MaxFileBytes+1)); err != nil {
		return FileEntry{}, err
	}
	return FileEntry{Path: filepath.Base(path), Size: fi.Size(), SHA: hex.EncodeToString(h.Sum(nil))}, nil
}

// HashTree hashes every regular file under root (relative paths, sorted)
// so the digest is order-independent and reproducible.
func HashTree(root string) ([]FileEntry, error) {
	var entries []FileEntry
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		e, err := HashFile(p)
		if err != nil {
			return err
		}
		e.Path = filepath.ToSlash(rel)
		entries = append(entries, e)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries, nil
}

// Digest folds files, config and parser version into one cache key.
func Digest(in DigestInput) string {
	h := sha256.New()
	fmt.Fprintf(h, "parser=%d\nconfig=%s\n", in.ParserVersion, in.Config)
	for _, f := range in.Files {
		fmt.Fprintf(h, "%s %d %s\n", f.Path, f.Size, f.SHA)
	}
	return hex.EncodeToString(h.Sum(nil))
}
