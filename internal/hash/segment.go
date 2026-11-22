// Package segmenthash computes per-segment SHA-256 digests over both input
// bytes and normalized output. Digests are the evidence that a rewrap is
// reproducible: identical inputs must yield identical hashes across runs.
package segmenthash

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"sort"
)

// Size is the digest size in bytes.
const Size = sha256.Size

// Hex is a lowercase hex SHA-256 digest.
type Hex string

// Digest hashes r completely and returns the hex digest.
func Digest(r io.Reader) (Hex, error) {
	if r == nil {
		return "", fmt.Errorf("segmenthash: nil reader")
	}
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", fmt.Errorf("segmenthash: read: %w", err)
	}
	return Hex(hex.EncodeToString(h.Sum(nil))), nil
}

// Bytes hashes a byte slice.
func Bytes(b []byte) Hex {
	sum := sha256.Sum256(b)
	return Hex(hex.EncodeToString(sum[:]))
}

// Writer is an incremental hash writer.
type Writer struct {
	h hash.Hash
}

// NewWriter returns an incremental digest writer.
func NewWriter() *Writer { return &Writer{h: sha256.New()} }

// Write adds bytes to the digest.
func (w *Writer) Write(p []byte) (int, error) { return w.h.Write(p) }

// WriteString adds a string to the digest.
func (w *Writer) WriteString(s string) (int, error) { return io.WriteString(w.h, s) }

// Sum returns the digest of everything written so far.
func (w *Writer) Sum() Hex { return Hex(hex.EncodeToString(w.h.Sum(nil))) }

// Record is one segment's evidence.
type Record struct {
	URI        string `json:"uri"`
	Sequence   uint64 `json:"sequence"`
	Role       string `json:"role"` // "input" or "output"
	Chunk      int    `json:"chunk"`
	Size       int64  `json:"size"`
	SHA256     Hex    `json:"sha256"`
	StartTicks int64  `json:"start_ticks,omitempty"`
	EndTicks   int64  `json:"end_ticks,omitempty"`
	// Gap marks a preserved timeline hole; Determined distinguishes a
	// confirmed keyframe timeline from an unscannable segment. Both are
	// persisted so a resumed run reports identical statistics.
	Gap        bool `json:"gap,omitempty"`
	Determined bool `json:"determined,omitempty"`
}

// Validate checks a record is internally consistent.
func (r Record) Validate() error {
	if r.URI == "" {
		return fmt.Errorf("segmenthash: record missing uri")
	}
	if r.Role != "input" && r.Role != "output" {
		return fmt.Errorf("segmenthash: record %s has invalid role %q", r.URI, r.Role)
	}
	if len(r.SHA256) != 64 {
		return fmt.Errorf("segmenthash: record %s has malformed digest", r.URI)
	}
	if r.Size < 0 {
		return fmt.Errorf("segmenthash: record %s has negative size", r.URI)
	}
	return nil
}

// Compare counts matching output records against the expected input mapping.
// The mapping is keyed by output URI; each expected value is the input URI
// whose content the output must reproduce. Missing or mismatched entries are
// reported so the caller can compute a match ratio.
type Comparison struct {
	Matched  int      `json:"matched"`
	Total    int      `json:"total"`
	Missing  []string `json:"missing,omitempty"`
	Mismatch []string `json:"mismatch,omitempty"`
}

// Ratio returns matched/total as a percentage, or 0 when total is zero.
func (c Comparison) Ratio() float64 {
	if c.Total == 0 {
		return 0
	}
	return 100 * float64(c.Matched) / float64(c.Total)
}

// Compare pairs output records with reference records by URI.
func Compare(reference, output []Record) Comparison {
	ref := make(map[string]Record, len(reference))
	for _, r := range reference {
		ref[r.URI] = r
	}
	c := Comparison{Total: len(reference)}
	for _, out := range output {
		want, ok := ref[out.URI]
		if !ok {
			c.Mismatch = append(c.Mismatch, out.URI)
			continue
		}
		if want.SHA256 == out.SHA256 && want.Size == out.Size {
			c.Matched++
		} else {
			c.Mismatch = append(c.Mismatch, out.URI)
		}
	}
	for _, r := range reference {
		found := false
		for _, out := range output {
			if out.URI == r.URI {
				found = true
				break
			}
		}
		if !found {
			c.Missing = append(c.Missing, r.URI)
		}
	}
	sort.Strings(c.Missing)
	sort.Strings(c.Mismatch)
	return c
}

// Manifest sorts records by URI then role for deterministic serialisation.
func Manifest(records []Record) []Record {
	out := make([]Record, len(records))
	copy(out, records)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].URI != out[j].URI {
			return out[i].URI < out[j].URI
		}
		return out[i].Role < out[j].Role
	})
	return out
}
