package hls

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// File is one rendered playlist or segment payload awaiting publication.
type File struct {
	RelPath string
	Content []byte
}

// Publish writes every file under dir atomically: each file is written to
// a sibling temp file and renamed into place, so a crash never leaves a
// half-written playlist. The directory is created if missing.
func Publish(dir string, files []File) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create output dir: %w", err)
	}
	// Deterministic write order keeps directory traversal reproducible.
	sorted := append([]File(nil), files...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].RelPath < sorted[j].RelPath })
	for _, f := range sorted {
		full := filepath.Join(dir, f.RelPath)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		tmp := full + ".tmp"
		if err := os.WriteFile(tmp, f.Content, 0o644); err != nil {
			return err
		}
		if err := os.Rename(tmp, full); err != nil {
			return err
		}
	}
	return nil
}

// ManifestChecksum hashes every published file in a fixed order so that a
// re-run with identical inputs yields an identical digest. The file
// relative path is mixed in alongside the content to detect renames.
func ManifestChecksum(files []File) string {
	sorted := append([]File(nil), files...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].RelPath < sorted[j].RelPath })
	h := sha256.New()
	for _, f := range sorted {
		h.Write([]byte(f.RelPath))
		h.Write([]byte{0})
		h.Write(f.Content)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ChecksumBytes is a standalone helper used to verify an already-written
// master playlist against an expected digest.
func ChecksumBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
