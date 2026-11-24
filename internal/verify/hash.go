// Package verify computes per-segment checksums and re-reads delivered
// artifacts to prove they still match the manifest.
package verify

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"mediarun/internal/model"
)

// BytesHash is the canonical 64-character lowercase hex SHA-256.
func BytesHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// FileHash hashes a file on disk without loading it whole.
func FileHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := copyTo(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Result is the outcome of verifying one segment.
type Result struct {
	Name     string `json:"name"`
	Expected string `json:"expected"`
	Actual   string `json:"actual"`
	OK       bool   `json:"ok"`
	Missing  bool   `json:"missing,omitempty"`
}

// Report is the aggregate verification outcome.
type Report struct {
	Results []Result `json:"results"`
	Missing []string `json:"missing,omitempty"`
}

// Matched reports how many segments hash-match the manifest.
func (r Report) Matched() int {
	n := 0
	for _, res := range r.Results {
		if res.OK {
			n++
		}
	}
	return n
}

// AllOK is true only when every expected segment exists and matches.
func (r Report) AllOK() bool {
	if len(r.Results) == 0 || len(r.Missing) != 0 {
		return false
	}
	for _, res := range r.Results {
		if !res.OK {
			return false
		}
	}
	return true
}

// SegmentExpected pairs a segment name with its committed hash.
type SegmentExpected struct {
	Name string
	Hash string
}

// Segments recomputes each committed segment's hash from disk and compares.
func Segments(dir string, expected []SegmentExpected) (Report, error) {
	var report Report
	for _, e := range expected {
		if len(e.Hash) != 64 {
			return report, fmt.Errorf("%w: segment %q expected hash %q", model.ErrInvalid, e.Name, e.Hash)
		}
		if _, err := hex.DecodeString(e.Hash); err != nil {
			return report, fmt.Errorf("%w: segment %q expected hash %q", model.ErrInvalid, e.Name, e.Hash)
		}
		path := filepath.Join(dir, "seg-"+e.Name+".json")
		actual, err := FileHash(path)
		if err != nil {
			if os.IsNotExist(err) {
				report.Missing = append(report.Missing, e.Name)
				report.Results = append(report.Results, Result{Name: e.Name, Expected: e.Hash, Missing: true})
				continue
			}
			return report, err
		}
		report.Results = append(report.Results, Result{
			Name:     e.Name,
			Expected: e.Hash,
			Actual:   actual,
			OK:       actual == e.Hash,
		})
	}
	sort.SliceStable(report.Results, func(i, j int) bool { return report.Results[i].Name < report.Results[j].Name })
	return report, nil
}

// copyTo streams src into dst.
func copyTo(dst io.Writer, src io.Reader) (int64, error) {
	return io.Copy(dst, src)
}
