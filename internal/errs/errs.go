// Package errs defines the error classification and location fields shared by
// the clock, index, stream and segment modules. A single vocabulary keeps the
// CLI exit codes, the recovery report and the resume state consistent.
package errs

import (
	"fmt"
	"sort"
	"strings"
)

// Class is the coarse, stable error family reported to operators.
type Class string

const (
	ClassTruncated   Class = "truncated"
	ClassBadLength   Class = "bad_length"
	ClassOutOfRange  Class = "out_of_range"
	ClassCorrupt     Class = "corrupt"
	ClassDependency  Class = "missing_dependency"
	ClassCycle       Class = "dependency_cycle"
	ClassDigest      Class = "digest_mismatch"
	ClassClock       Class = "clock_discontinuity"
	ClassUnsupported Class = "unsupported"
	ClassUsage       Class = "usage"
	ClassInternal    Class = "internal"
)

// Stable issue codes. Reports and tests match on these, never on prose.
const (
	CodeNALStartCodeTruncated = "nal.start_code_truncated"
	CodeNALHeaderTruncated    = "nal.header_truncated"
	CodeNALLengthZero         = "nal.length_zero"
	CodeNALLengthOverrun      = "nal.length_overrun"
	CodeNALUnknownCodec       = "nal.unknown_codec"
	CodeOBUHeaderTruncated    = "av1.obu_header_truncated"
	CodeOBULengthOverrun      = "av1.obu_length_overrun"
	CodeOBULengthZero         = "av1.obu_length_zero"

	CodeIndexHeaderTruncated  = "index.header_truncated"
	CodeIndexHeaderBadMagic   = "index.header_bad_magic"
	CodeIndexEntryOverrun     = "index.entry_overrun"
	CodeIndexOffsetOutOfRange = "index.offset_out_of_range"
	CodeIndexOffsetOverlap    = "index.offset_overlap"
	CodeIndexDuplicateSample  = "index.duplicate_sample"
	CodeIndexMissingPage      = "index.missing_page"
	CodeIndexPTSRegressed     = "index.pts_regressed"
	CodeIndexTooLarge         = "index.too_large"
	CodeIndexRepaired         = "index.repaired_from_scan"
	CodeIndexPendingReview    = "index.boundary_pending_review"

	CodeClockWrap          = "clock.wrap_unwrapped"
	CodeClockBackward      = "clock.timestamp_backward"
	CodeClockRepeat        = "clock.timestamp_repeat"
	CodeClockDriftBreak    = "clock.drift_break"
	CodeClockExtrapolation = "clock.extrapolation_limited"
	CodeClockAnchorBad     = "clock.anchor_invalid"

	CodeDependencyMissing = "graph.dependency_missing"
	CodeDependencyCycle   = "graph.cycle"
	CodeDependencyStopped = "graph.branch_stopped"

	CodeSegmentBoundary = "segment.boundary_deviation"
	CodeAlignmentDelta  = "align.max_delta"

	CodeDigestInput  = "resume.input_digest_mismatch"
	CodeDigestConfig = "resume.config_digest_mismatch"

	CodeRTPUnrecoverable = "rtp.unrecoverable_gap"
)

// Location pinpoints an issue in one input artifact.
type Location struct {
	File    string `json:"file,omitempty"`
	TrackID uint32 `json:"track_id,omitempty"`
	Kind    string `json:"track_kind,omitempty"`
	Sample  int    `json:"sample"` // -1 when not applicable
	Offset  int64  `json:"offset"` // -1 when not applicable
	Field   string `json:"field,omitempty"`
}

// String renders a location compactly and deterministically.
func (l Location) String() string {
	var b strings.Builder
	if l.File != "" {
		b.WriteString(l.File)
	}
	if l.TrackID != 0 {
		if b.Len() > 0 {
			b.WriteByte('/')
		}
		fmt.Fprintf(&b, "track=%d", l.TrackID)
	}
	if l.Sample >= 0 {
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "sample=%d", l.Sample)
	}
	if l.Offset >= 0 {
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "offset=%d", l.Offset)
	}
	if l.Field != "" {
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "field=%s", l.Field)
	}
	if b.Len() == 0 {
		return "n/a"
	}
	return b.String()
}

// UnknownLocation is the neutral location used when neither sample nor offset
// is meaningful.
func UnknownLocation() Location { return Location{Sample: -1, Offset: -1} }

// Issue is one machine-readable finding.
type Issue struct {
	Class   Class    `json:"class"`
	Code    string   `json:"code"`
	Message string   `json:"message"`
	Loc     Location `json:"location"`
	Fatal   bool     `json:"fatal"`
}

func (i Issue) Error() string {
	if i.Loc.File != "" || i.Loc.TrackID != 0 || i.Loc.Sample >= 0 || i.Loc.Offset >= 0 {
		return fmt.Sprintf("%s (%s): %s", i.Code, i.Loc.String(), i.Message)
	}
	return fmt.Sprintf("%s: %s", i.Code, i.Message)
}

// List accumulates issues in insertion order; Sort gives stable report output.
type List []Issue

// Add appends an issue. sample and offset use -1 for "not applicable".
func (l *List) Add(class Class, code, msg string, loc Location) {
	l.append(Issue{Class: class, Code: code, Message: msg, Loc: loc})
}

// AddFatal appends an issue that stops the affected branch.
func (l *List) AddFatal(class Class, code, msg string, loc Location) {
	l.append(Issue{Class: class, Code: code, Message: msg, Loc: loc, Fatal: true})
}

func (l *List) append(i Issue) {
	if i.Loc.Sample == 0 && i.Loc.Offset == 0 && i.Loc.Field == "" {
		// Callers that only set a file/track still get a coherent location.
		i.Loc.Sample = -1
		i.Loc.Offset = -1
	}
	*l = append(*l, i)
}

// Sort orders issues deterministically by track, offset, sample and code.
func (l List) Sort() {
	sort.SliceStable(l, func(i, j int) bool {
		a, b := l[i], l[j]
		if a.Loc.TrackID != b.Loc.TrackID {
			return a.Loc.TrackID < b.Loc.TrackID
		}
		if a.Loc.Offset != b.Loc.Offset {
			return a.Loc.Offset < b.Loc.Offset
		}
		if a.Loc.Sample != b.Loc.Sample {
			return a.Loc.Sample < b.Loc.Sample
		}
		return a.Code < b.Code
	})
}

// CountByClass returns counts keyed by class for metrics and reports.
func (l List) CountByClass() map[string]int {
	out := map[string]int{}
	for _, i := range l {
		out[string(i.Class)]++
	}
	return out
}

// HasFatal reports whether any issue is marked fatal.
func (l List) HasFatal() bool {
	for _, i := range l {
		if i.Fatal {
			return true
		}
	}
	return false
}

// FatalTracks returns the set of track IDs with fatal issues, sorted.
func (l List) FatalTracks() []uint32 {
	seen := map[uint32]bool{}
	for _, i := range l {
		if i.Fatal {
			seen[i.Loc.TrackID] = true
		}
	}
	out := make([]uint32, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Slice(out, func(a, b int) bool { return out[a] < out[b] })
	return out
}

// Summarize renders a compact, deterministic one-line description of codes.
func (l List) Summarize() string {
	if len(l) == 0 {
		return "no issues"
	}
	parts := make([]string, 0, len(l))
	seen := map[string]bool{}
	for _, i := range l {
		if seen[i.Code] {
			continue
		}
		seen[i.Code] = true
		parts = append(parts, i.Code)
	}
	sort.Strings(parts)
	return fmt.Sprintf("%d issues: %s", len(l), strings.Join(parts, ", "))
}
