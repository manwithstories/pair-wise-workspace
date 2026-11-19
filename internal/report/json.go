// Package report serializes pipeline results as stable, machine-readable JSON.
// Field names and ordering are part of the interface: automated acceptance
// reads them, so they must not change between runs with identical inputs.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"archive.local/recover/internal/errs"
)

// SchemaVersion is bumped only for breaking changes to the JSON shape.
const SchemaVersion = 1

// Metrics carries the acceptance figures for a demo run.
type Metrics struct {
	SyncErrorMs      float64 `json:"sync_error_ms"`
	LossRecoveryRate float64 `json:"loss_recovery_rate"`
	P95LatencyMs     float64 `json:"p95_latency_ms"`
	Segments         int     `json:"segments"`
	AlignmentDeltaMs float64 `json:"alignment_delta_ms"`
}

// Report is the top-level document written to stdout or a file.
type Report struct {
	SchemaVersion int             `json:"schema_version"`
	RunID         string          `json:"run_id"`
	Case          string          `json:"case,omitempty"`
	Seed          int64           `json:"seed"`
	OK            bool            `json:"ok"`
	ExitCode      int             `json:"exit_code"`
	Metrics       Metrics         `json:"metrics"`
	Timeline      *TimelineReport `json:"timeline,omitempty"`
	Streams       []StreamSummary `json:"streams,omitempty"`
	Index         *IndexReport    `json:"index,omitempty"`
	Tracks        *TracksReport   `json:"tracks,omitempty"`
	Segments      []SegmentReport `json:"segment_list,omitempty"`
	Alignment     *AlignReport    `json:"alignment,omitempty"`
	RTP           *RTPReport      `json:"rtp,omitempty"`
	Resume        *ResumeReport   `json:"resume,omitempty"`
	Issues        errs.List       `json:"issues"`
	IssueCounts   map[string]int  `json:"issue_counts"`
}

// TimelineReport summarizes the recovered program timeline.
type TimelineReport struct {
	DurationNs int64 `json:"duration_ns"`
	Tracks     int   `json:"tracks"`
	Samples    int   `json:"samples"`
}

// StreamSummary is one scanned bitstream.
type StreamSummary struct {
	File      string `json:"file"`
	Codec     string `json:"codec"`
	Units     int    `json:"units"`
	Keyframes int    `json:"keyframes"`
	Bytes     int    `json:"bytes"`
	Truncated bool   `json:"truncated"`
}

// IndexReport summarizes index validation and repair.
type IndexReport struct {
	File     string `json:"file"`
	Original int    `json:"original_entries"`
	Repaired int    `json:"repaired_entries"`
	Pending  int    `json:"pending_review"`
	Dropped  int    `json:"dropped_entries"`
	Missing  int    `json:"missing_pages"`
}

// TracksReport summarizes the dependency plan. Each rendition contributes its
// own plan, so the per-rendition orders are listed separately rather than
// concatenated, which would be ambiguous.
type TracksReport struct {
	Plans   []TrackPlan   `json:"plans"`
	Stopped []StoppedInfo `json:"stopped"`
}

// TrackPlan is one rendition's dependency order.
type TrackPlan struct {
	Rendition string   `json:"rendition"`
	Order     []uint32 `json:"order"`
}

// StoppedInfo names a branch that could not be recovered.
type StoppedInfo struct {
	TrackID uint32   `json:"track_id"`
	Kind    string   `json:"kind"`
	Code    string   `json:"code"`
	Reason  string   `json:"reason"`
	Related []uint32 `json:"related_tracks,omitempty"`
}

// SegmentReport is one entry of the multi-rendition segment manifest.
type SegmentReport struct {
	Index       int                         `json:"index"`
	StartNs     int64                       `json:"start_ns"`
	EndNs       int64                       `json:"end_ns"`
	DeviationMs float64                     `json:"deviation_ms"`
	Pending     bool                        `json:"pending_review"`
	Renditions  map[string]RenditionSegment `json:"renditions"`
}

// RenditionSegment is one rendition's view of a shared segment.
type RenditionSegment struct {
	Bitrate        int   `json:"bitrate"`
	StartNs        int64 `json:"start_ns"`
	KeyframeSample int   `json:"keyframe_sample"`
}

// AlignReport summarizes the ladder reconciliation.
type AlignReport struct {
	MaxDeltaNs float64 `json:"alignment_delta_ms"`
	Pending    int     `json:"pending_review"`
	Boundaries int     `json:"boundaries"`
}

// RTPReport summarizes loss recovery.
type RTPReport struct {
	Received     int     `json:"received"`
	Duplicates   int     `json:"duplicates"`
	Reordered    int     `json:"reordered"`
	Unique       int     `json:"unique_packets"`
	Gaps         int     `json:"gaps"`
	Lost         int     `json:"lost_packets"`
	RecoveryRate float64 `json:"loss_recovery_rate"`
	PayloadBytes int     `json:"recovered_payload_bytes"`
}

// ResumeReport records whether a previous run was reused.
type ResumeReport struct {
	Used           bool   `json:"used"`
	InputDigest    string `json:"input_digest"`
	ConfigDigest   string `json:"config_digest"`
	PreviousInput  string `json:"previous_input_digest,omitempty"`
	PreviousConfig string `json:"previous_config_digest,omitempty"`
	Reason         string `json:"reason,omitempty"`
}

// New builds a report with a deterministic run ID from the run parameters.
func New(caseName string, seed int64) *Report {
	return &Report{
		SchemaVersion: SchemaVersion,
		RunID:         fmt.Sprintf("%s-seed%d", caseName, seed),
		Case:          caseName,
		Seed:          seed,
		IssueCounts:   map[string]int{},
	}
}

// Finalize sorts issues, recomputes counts and derives the exit code.
// Finalize sorts issues, recomputes counts and derives the exit code. OK means
// the run completed without unrecoverable errors; a partial run that still
// produced a report is not OK, so automation reading only the field is never
// misled by a nonzero exit code.
func (r *Report) Finalize() {
	r.Issues.Sort()
	r.IssueCounts = r.Issues.CountByClass()
	r.ExitCode = ExitCodeFor(r)
	r.OK = r.ExitCode == ExitOK
}

// ExitCodes are the program's stable contract with automation.
const (
	ExitOK            = 0
	ExitUsage         = 2
	ExitPartial       = 3
	ExitUnrecoverable = 4
)

// ExitCodeFor maps the report state to the process exit code.
//
//	0: processed completely
//	2: usage error (caller sets it before Finalize)
//	3: corrupt input, partial report still produced
//	4: unrecoverable dependency or internal failure
func ExitCodeFor(r *Report) int {
	if r.ExitCode == ExitUsage {
		return ExitUsage
	}
	hasUnrecoverable := false
	hasCorrupt := false
	for _, i := range r.Issues {
		switch i.Class {
		case errs.ClassDependency, errs.ClassCycle, errs.ClassInternal, errs.ClassDigest:
			hasUnrecoverable = true
		case errs.ClassCorrupt, errs.ClassTruncated, errs.ClassBadLength, errs.ClassOutOfRange:
			hasCorrupt = true
		}
	}
	switch {
	case hasUnrecoverable:
		return ExitUnrecoverable
	case hasCorrupt:
		return ExitPartial
	default:
		return ExitOK
	}
}

// Write emits compact, deterministic JSON with a trailing newline.
func Write(w io.Writer, r *Report) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// Marshal returns the report as pretty JSON bytes.
func Marshal(r *Report) ([]byte, error) {
	return json.MarshalIndent(r, "", "  ")
}

// SortedIssueCodes returns distinct issue codes in ascending order. Useful for
// tests and human summaries that must not depend on insertion order.
func (r *Report) SortedIssueCodes() []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(r.Issues))
	for _, i := range r.Issues {
		if !seen[i.Code] {
			seen[i.Code] = true
			out = append(out, i.Code)
		}
	}
	sort.Strings(out)
	return out
}
