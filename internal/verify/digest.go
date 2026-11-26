// Package verify provides repeatable CRC32 and SHA-256 verification.
package verify

import (
	"crypto/sha256"
	"encoding/hex"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// CRC32IEEE returns the IEEE CRC-32 used by NDJSON records and chunks.
func CRC32IEEE(data []byte) uint32 {
	return crc32.ChecksumIEEE(data)
}

// CRC32Hex returns an eight-character lowercase CRC string.
func CRC32Hex(data []byte) string {
	return hex.EncodeToString([]byte{
		byte(CRC32IEEE(data) >> 24),
		byte(CRC32IEEE(data) >> 16),
		byte(CRC32IEEE(data) >> 8),
		byte(CRC32IEEE(data)),
	})
}

// SHA256Bytes returns the lowercase 64-character digest of data.
func SHA256Bytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// SHA256File streams a file so large chunks are not fully retained.
func SHA256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// CRC32File streams a file through IEEE CRC-32.
func CRC32File(path string) (uint32, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	h := crc32.NewIEEE()
	if _, err := io.Copy(h, f); err != nil {
		return 0, err
	}
	return h.Sum32(), nil
}

// DigestTree computes a stable digest over selected files beneath root.
// Paths are slash-normalized and sorted; root itself never enters the hash,
// which makes the result independent of the temporary demo directory.
func DigestTree(root string, match func(rel string) bool) (string, error) {
	type item struct {
		rel  string
		path string
	}
	var files []item
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if match != nil && !match(rel) {
			return nil
		}
		files = append(files, item{rel: rel, path: path})
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].rel < files[j].rel })

	h := sha256.New()
	for _, f := range files {
		sum, err := SHA256File(f.path)
		if err != nil {
			return "", err
		}
		// Length-prefix the path and digest to avoid canonicalization ambiguity.
		_, _ = io.WriteString(h, f.rel)
		_, _ = h.Write([]byte{0})
		_, _ = io.WriteString(h, strings.TrimSpace(sum))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ManifestDigestMatcher selects deterministic relay output artifacts.
func ManifestDigestMatcher(rel string) bool {
	switch {
	case strings.HasSuffix(rel, ".m3u8"):
		return true
	case strings.HasPrefix(rel, "segments/") && strings.HasSuffix(rel, ".ts"):
		return true
	case strings.HasPrefix(rel, "chunks/") && strings.HasSuffix(rel, ".ndjson"):
		return true
	case rel == "checkpoint/index.json":
		return true
	default:
		return false
	}
}
