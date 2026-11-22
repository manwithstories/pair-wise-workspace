// Package manifest aligns multiple bitrate representations onto one
// common time grid and produces deterministic, checksummed segment
// manifests. Missing tracks, empty segments and boundaries that cannot
// be matched are recorded as structured errors rather than dropped.
package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

// Segment is one aligned output segment on the common grid.
type Segment struct {
	Index          int    `json:"index"`
	StartNS        int64  `json:"start_ns"`
	EndNS          int64  `json:"end_ns"`
	Representation string `json:"representation"`
	Tracks         []uint64 `json:"tracks"`
	// Blocks counts the source blocks absorbed into this segment.
	Blocks int `json:"blocks"`
	// Bytes sums the source payload bytes referenced.
	Bytes int64 `json:"bytes"`
	// Empty marks a grid cell with no matching source block.
	Empty bool `json:"empty,omitempty"`
}

// Representation is one bitrate layer built from a set of tracks.
type Representation struct {
	ID     string   `json:"id"`
	Tracks []uint64 `json:"tracks"`
	// Bitratebps is informational; alignment only uses time boundaries.
	Bitratebps int `json:"bitrate_bps"`
}

// StructuredError records a non-fatal alignment problem with enough
// context to locate it (representation, grid boundary, track).
type StructuredError struct {
	Kind    string `json:"kind"` // missing_track | empty_segment | unmatched_boundary
	Rep     string `json:"rep,omitempty"`
	Track   uint64 `json:"track,omitempty"`
	StartNS int64  `json:"start_ns,omitempty"`
	Detail  string `json:"detail"`
}

// Manifest is the complete aligned output for one source.
type Manifest struct {
	SourceFingerprint string             `json:"source_fingerprint"`
	GridNS            int64              `json:"grid_ns"`
	Representations   []Representation   `json:"representations"`
	Segments          []Segment          `json:"segments"`
	Errors            []StructuredError  `json:"errors,omitempty"`
}

// CanonicalBytes serializes the manifest deterministically: JSON keys in
// struct order, no map iteration, and a trailing newline so the digest
// is stable across machines and repeated runs.
func CanonicalBytes(m *Manifest) ([]byte, error) {
	cp := *m
	// Defensive copies so external mutation cannot change the digest.
	cp.Segments = append([]Segment(nil), m.Segments...)
	cp.Representations = append([]Representation(nil), m.Representations...)
	cp.Errors = append([]StructuredError(nil), m.Errors...)
	for i := range cp.Segments {
		cp.Segments[i].Tracks = append([]uint64(nil), m.Segments[i].Tracks...)
	}
	raw, err := json.MarshalIndent(&cp, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

// Checksum returns the lowercase hex SHA-256 of the canonical encoding
// (always 64 hex characters).
func Checksum(m *Manifest) (string, error) {
	raw, err := CanonicalBytes(m)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// FingerprintBytes returns the SHA-256 of arbitrary source bytes, used
// for the input fingerprint stored in the checkpoint.
func FingerprintBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// sortedCopy returns a sorted copy of a uint64 slice.
func sortedCopy(in []uint64) []uint64 {
	out := append([]uint64(nil), in...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
