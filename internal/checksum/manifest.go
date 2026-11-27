// Package checksum produces the deterministic delivery manifest. Every
// normalized artifact is hashed with SHA-256 and listed in a stable order,
// so repeat runs over identical inputs yield byte-identical manifests and
// therefore identical overall digests.
package checksum

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// FileEntry is one artifact row in the manifest.
type FileEntry struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Manifest is the canonical, JSON-serializable delivery record.
type Manifest struct {
	SchemaVersion int         `json:"schema_version"`
	Files         []FileEntry `json:"files"`
	// ManifestDigest hashes the canonical file list, independent of FS order.
	ManifestDigest string `json:"manifest_digest"`
}

// SchemaVersion is bumped only when the on-disk manifest format changes.
const SchemaVersion = 1

// HashBytes returns the lowercase hex SHA-256 of data.
func HashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Build walks dir (excluding the manifest file itself), hashes every regular
// file and assembles the canonical manifest. The result is deterministic:
// rows sort by relative path and the digest covers the sorted representation.
func Build(dir, manifestName string) (Manifest, error) {
	var files []FileEntry
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if rel == manifestName {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files = append(files, FileEntry{
			Path: filepath.ToSlash(rel), Size: info.Size(), SHA256: HashBytes(raw),
		})
		return nil
	})
	if err != nil {
		return Manifest{}, fmt.Errorf("checksum: walk: %w", err)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })

	m := Manifest{SchemaVersion: SchemaVersion, Files: files}
	canonical, err := json.Marshal(m)
	if err != nil {
		return Manifest{}, fmt.Errorf("checksum: marshal: %w", err)
	}
	m.ManifestDigest = HashBytes(canonical)
	return m, nil
}

// Write serializes the manifest to dir/manifestName with stable indentation.
func Write(dir, manifestName string, m Manifest) (string, error) {
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", fmt.Errorf("checksum: marshal manifest: %w", err)
	}
	path := filepath.Join(dir, manifestName)
	if err := os.WriteFile(path, append(raw, '\n'), 0o644); err != nil {
		return "", fmt.Errorf("checksum: write manifest: %w", err)
	}
	return path, nil
}

// Verify recomputes the manifest over dir and compares every digest plus the
// overall manifest digest, returning the first mismatch as an error.
func Verify(dir, manifestName string) error {
	want, err := readManifest(dir, manifestName)
	if err != nil {
		return err
	}
	got, err := Build(dir, manifestName)
	if err != nil {
		return err
	}
	if len(got.Files) != len(want.Files) {
		return fmt.Errorf("checksum: file count %d != recorded %d", len(got.Files), len(want.Files))
	}
	for i := range want.Files {
		g, w := got.Files[i], want.Files[i]
		if g.Path != w.Path || g.SHA256 != w.SHA256 || g.Size != w.Size {
			return fmt.Errorf("checksum: mismatch on %q", w.Path)
		}
	}
	if got.ManifestDigest != want.ManifestDigest {
		return fmt.Errorf("checksum: manifest digest mismatch")
	}
	return nil
}

func readManifest(dir, name string) (Manifest, error) {
	raw, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return Manifest{}, fmt.Errorf("checksum: read manifest: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return Manifest{}, fmt.Errorf("checksum: parse manifest: %w", err)
	}
	return m, nil
}
