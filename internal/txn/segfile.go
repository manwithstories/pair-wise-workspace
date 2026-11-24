package txn

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"
)

// ReadSegment reads a committed segment and returns its bytes and hash.
func ReadSegment(dir, name string) ([]byte, string, error) {
	data, err := os.ReadFile(filepath.Join(dir, SegmentFile(name)))
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(data)
	return data, hex.EncodeToString(sum[:]), nil
}

// RemoveSegmentsNotIn deletes segment files that the manifest no longer
// covers. It only removes files matching the segment naming pattern.
func RemoveSegmentsNotIn(dir string, m *Manifest) error {
	keep := map[string]bool{}
	for _, s := range m.Segments {
		keep[SegmentFile(s.Name)] = true
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var names []string
	for _, e := range entries {
		n := e.Name()
		if len(n) < 8 || n[:4] != "seg-" || filepath.Ext(n) != ".json" {
			continue
		}
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if keep[n] {
			continue
		}
		if err := os.Remove(filepath.Join(dir, n)); err != nil {
			return err
		}
	}
	return nil
}
