package pack

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"example.com/gkpack/internal/chunker"
	"example.com/gkpack/internal/manifest"
	"example.com/gkpack/internal/store"
)

// ScanStats summarizes a source tree scan.
type ScanStats struct {
	Files        int
	RawBytes     uint64
	Chunks       int
	UniqueBlocks int
}

// ScanSource recursively reads regular files in lexical path order. Symlinks
// and other special files are ignored. Chunks are inserted into st at once, so
// scanning uses bounded memory regardless of total resource size.
func ScanSource(src string, st *store.Store) (manifest.Manifest, ScanStats, error) {
	info, err := os.Stat(src)
	if err != nil {
		return manifest.Manifest{}, ScanStats{}, fmt.Errorf("source: %w", err)
	}
	if !info.IsDir() {
		return manifest.Manifest{}, ScanStats{}, fmt.Errorf("source %q is not a directory", src)
	}

	var paths []string
	if err := filepath.WalkDir(src, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == src {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		paths = append(paths, filepath.ToSlash(rel))
		return nil
	}); err != nil {
		return manifest.Manifest{}, ScanStats{}, err
	}
	sort.Strings(paths)

	m := manifest.New()
	var stats ScanStats
	for _, rel := range paths {
		full := filepath.Join(src, filepath.FromSlash(rel))
		f, err := os.Open(full)
		if err != nil {
			return manifest.Manifest{}, stats, err
		}
		entry := manifest.File{Path: rel, Blocks: []manifest.BlockRef{}}
		hasher := sha256.New()
		var offset uint64
		splitter := chunker.New(f)
		for {
			chunk, err := splitter.Next()
			if err != nil {
				if err == io.EOF {
					break
				}
				_ = f.Close()
				return manifest.Manifest{}, stats, fmt.Errorf("%s: %w", rel, err)
			}
			sum := sha256.Sum256(chunk)
			hash := hex.EncodeToString(sum[:])
			if st != nil {
				if _, err := st.Put(chunk); err != nil {
					_ = f.Close()
					return manifest.Manifest{}, stats, fmt.Errorf("%s: store block: %w", rel, err)
				}
			}
			_, _ = hasher.Write(chunk)
			entry.Blocks = append(entry.Blocks, manifest.BlockRef{
				Hash:   hash,
				Offset: offset,
				Size:   uint32(len(chunk)),
			})
			offset += uint64(len(chunk))
			stats.Chunks++
		}
		if err := f.Close(); err != nil {
			return manifest.Manifest{}, stats, err
		}
		entry.Size = offset
		entry.SHA256 = hex.EncodeToString(hasher.Sum(nil))
		if entry.Size == 0 {
			entry.SHA256 = manifest.EmptySHA256
		}
		m.Files = append(m.Files, entry)
		stats.RawBytes += entry.Size
	}
	m.Finalize()
	if err := m.Validate(); err != nil {
		return manifest.Manifest{}, stats, err
	}
	stats.Files = len(m.Files)
	if st != nil {
		stats.UniqueBlocks = len(st.Entries())
	}
	return m, stats, nil
}

// ScanTree recomputes a tree manifest without writing a temporary block store.
// It is used for final output root verification.
func ScanTree(src string) (manifest.Manifest, error) {
	m, _, err := ScanSource(src, nil)
	return m, err
}

// CleanRelativePath validates a manifest path before it is joined to an output
// directory.
func CleanRelativePath(path string) (string, error) {
	if path == "" || strings.HasPrefix(path, "/") || strings.ContainsRune(path, '\x00') {
		return "", fmt.Errorf("unsafe path %q", path)
	}
	clean := filepath.Clean(filepath.FromSlash(path))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || filepath.IsAbs(clean) {
		return "", fmt.Errorf("unsafe path %q", path)
	}
	return clean, nil
}
