// Package verify turns pipeline outputs and their source evidence into a
// single reproducible report whose sha256 is identical across reruns.
package verify

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"mediad/internal/audio"
	"mediad/internal/fingerprint"
	"mediad/internal/segment"
)

// Exit codes shared with the CLI contract.
const (
	ExitOK          = 0
	ExitRecoverable = 2
	ExitFatal       = 1
)

// CutCount is the compact cut summary emitted in the report.
type CutCount struct {
	Track string `json:"track"`
	Count int    `json:"count"`
}

// BadOffset locates the first structural defect of a fragment.
type BadOffset struct {
	Track       string `json:"track"`
	Segment     int    `json:"segment"`
	Offset      int64  `json:"offset"`
	Recoverable bool   `json:"recoverable"`
	Op          string `json:"op"`
	Reason      string `json:"reason"`
}

// Report is the complete, JSON-serializable pipeline result. Field names are
// part of the CLI contract and must stay stable.
type Report struct {
	Segments       int                          `json:"segments"`
	Cuts           []CutCount                   `json:"cuts"`
	SilenceMS      []audio.Interval             `json:"silence_ms"`
	Duplicates     []fingerprint.DuplicateGroup `json:"duplicates"`
	FirstBadOffset *BadOffset                   `json:"first_bad_offset"`
	BlockedNode    string                       `json:"blocked_node"`
	ResumeMS       int64                        `json:"resume_ms"`
	ElapsedMS      int64                        `json:"elapsed_ms"`
	SHA256         string                       `json:"sha256"`
}

// Inputs collects every piece of evidence the report is built from.
type Inputs struct {
	Cuts       []segment.Cut
	Silence    []audio.Interval
	Duplicates []fingerprint.DuplicateGroup
	Bad        []BadOffset
	Blocked    string
	ResumeMS   int64
	ElapsedMS  int64
}

// Build assembles the report, sorts every slice for determinism and leaves
// SHA256 empty until Finalize is called.
func Build(in Inputs) *Report {
	r := &Report{
		SilenceMS:   append([]audio.Interval{}, in.Silence...),
		Duplicates:  append([]fingerprint.DuplicateGroup{}, in.Duplicates...),
		BlockedNode: in.Blocked,
		ResumeMS:    in.ResumeMS,
		ElapsedMS:   in.ElapsedMS,
	}
	trackCount := map[string]int{}
	for _, c := range in.Cuts {
		trackCount[string(c.Track)]++
	}
	for track, n := range trackCount {
		r.Cuts = append(r.Cuts, CutCount{Track: track, Count: n})
	}
	sort.Slice(r.Cuts, func(i, j int) bool { return r.Cuts[i].Track < r.Cuts[j].Track })
	r.Segments = len(in.Cuts)
	sort.Slice(r.SilenceMS, func(i, j int) bool {
		if r.SilenceMS[i].StartMS != r.SilenceMS[j].StartMS {
			return r.SilenceMS[i].StartMS < r.SilenceMS[j].StartMS
		}
		return r.SilenceMS[i].EndMS < r.SilenceMS[j].EndMS
	})
	sort.Slice(r.Duplicates, func(i, j int) bool { return r.Duplicates[i].DigestHex < r.Duplicates[j].DigestHex })
	sort.Slice(in.Bad, func(i, j int) bool {
		if in.Bad[i].Offset != in.Bad[j].Offset {
			return in.Bad[i].Offset < in.Bad[j].Offset
		}
		return in.Bad[i].Track < in.Bad[j].Track
	})
	if len(in.Bad) > 0 {
		b := in.Bad[0]
		r.FirstBadOffset = &b
	}
	return r
}

// canonicalReport contains only the stable content fields. Volatile timing
// metadata (ElapsedMS, ResumeMS) is deliberately excluded so the digest is
// identical across reruns even though wall-clock timings differ.
type canonicalReport struct {
	Segments       int                          `json:"segments"`
	Cuts           []CutCount                   `json:"cuts"`
	SilenceMS      []audio.Interval             `json:"silence_ms"`
	Duplicates     []fingerprint.DuplicateGroup `json:"duplicates"`
	FirstBadOffset *BadOffset                   `json:"first_bad_offset"`
	BlockedNode    string                       `json:"blocked_node"`
}

// canonical serializes the stable subset of the report, using a fixed field
// order (encoding/json emits struct fields in declaration order).
func (r *Report) canonical() ([]byte, error) {
	c := canonicalReport{
		Segments:       r.Segments,
		Cuts:           r.Cuts,
		SilenceMS:      r.SilenceMS,
		Duplicates:     r.Duplicates,
		FirstBadOffset: r.FirstBadOffset,
		BlockedNode:    r.BlockedNode,
	}
	data, err := json.MarshalIndent(&c, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// Finalize computes the stable digest over the canonical report and returns
// the hex string, also filling r.SHA256.
func (r *Report) Finalize() (string, error) {
	data, err := r.canonical()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	r.SHA256 = hex.EncodeToString(sum[:])
	return r.SHA256, nil
}

// WriteTo renders the finalized report as pretty JSON at path atomically.
func (r *Report) WriteTo(path string) error {
	if r.SHA256 == "" {
		if _, err := r.Finalize(); err != nil {
			return err
		}
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return nil
}

// Read parses a previously written report.
func Read(path string) (*Report, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r Report
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("verify: decode %s: %w", path, err)
	}
	return &r, nil
}

// Recheck recomputes the digest of a stored report and confirms it still
// matches its embedded sha256 — the reproducibility gate used by reruns.
func Recheck(path string) error {
	r, err := Read(path)
	if err != nil {
		return err
	}
	want := r.SHA256
	got, err := r.Finalize()
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("verify: report digest drifted: stored=%s recomputed=%s", want, got)
	}
	return nil
}
