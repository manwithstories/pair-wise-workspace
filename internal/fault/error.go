// Package fault defines the structured error model shared by every stage of
// the local media pipeline. A fault always carries three coordinates that
// together make a defect reproducible without re-reading the whole input:
//
//   - Stage: which stage detected it (parse, timeline, reorder, ...).
//   - Camera: which fixed camera produced the offending record.
//   - Offset: the raw byte offset of the record in its source file.
//
// Offsets are signed 64-bit values so callers can use -1 for "unknown" without
// wrapping. The package itself performs no I/O and has no dependencies.
package fault

import (
	"fmt"
	"sort"
	"strings"
)

// Stage identifies the pipeline stage that produced a fault.
type Stage string

const (
	StageParse     Stage = "parse"
	StageTimeline  Stage = "timeline"
	StageReorder   Stage = "reorder"
	StageTransform Stage = "transform"
	StageSegment   Stage = "segment"
	StageIndex     Stage = "index"
	StageVerify    Stage = "verify"
	StageIO        Stage = "io"
)

// Kind is the stable, machine-readable fault category. Strings are part of the
// on-disk report format; do not rename existing values.
type Kind string

const (
	KindUnknownVersion    Kind = "unknown_version"
	KindLengthOutOfBounds Kind = "length_out_of_bounds"
	KindChecksumMismatch  Kind = "checksum_mismatch"
	KindTruncatedRecord   Kind = "truncated_record"
	KindMalformedMetadata Kind = "malformed_metadata"
	KindTimestampJump     Kind = "timestamp_jump"
	KindDuplicateFrame    Kind = "duplicate_frame"
	KindLateFrame         Kind = "late_frame"
	KindMissingFrame      Kind = "missing_frame"
	KindSequenceWrap      Kind = "sequence_wrap"
	KindMissingKeyframe   Kind = "missing_keyframe"
	KindEmptySegment      Kind = "empty_segment"
	KindHashMismatch      Kind = "hash_mismatch"
	KindAlignmentMismatch Kind = "alignment_mismatch"
	KindIndexCorrupt      Kind = "index_corrupt"
	KindIndexBounds       Kind = "index_out_of_bounds"
	KindWriteFailure      Kind = "write_failure"
	KindCanceled          Kind = "canceled"
)

// Fault is one structured defect. Offset is the byte position of the record
// header (or of the segment file for post-processing faults). Fatal faults
// stop the affected branch; recoverable faults are recorded and processing
// continues with the remaining verifiable data.
type Fault struct {
	Kind     Kind   `json:"kind"`
	Stage    Stage  `json:"stage"`
	Camera   string `json:"camera,omitempty"`
	Seq      uint64 `json:"seq"`
	Offset   int64  `json:"offset"`
	File     string `json:"file,omitempty"`
	Message  string `json:"message"`
	Expected string `json:"expected,omitempty"`
	Actual   string `json:"actual,omitempty"`
	Fatal    bool   `json:"fatal"`
}

// Error renders the fault with every locating coordinate so log lines remain
// copy-pasteable into a bug report.
func (f Fault) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s at stage=%s", f.Kind, f.Stage)
	if f.Camera != "" {
		fmt.Fprintf(&b, " camera=%s", f.Camera)
	}
	fmt.Fprintf(&b, " seq=%d offset=%d", f.Seq, f.Offset)
	if f.File != "" {
		fmt.Fprintf(&b, " file=%s", f.File)
	}
	if f.Message != "" {
		fmt.Fprintf(&b, ": %s", f.Message)
	}
	if f.Expected != "" || f.Actual != "" {
		fmt.Fprintf(&b, " (expected %q, got %q)", f.Expected, f.Actual)
	}
	return b.String()
}

// List is an ordered collection of faults that also implements error.
type List []Fault

// Error summarises a list without losing the first locating coordinates.
func (l List) Error() string {
	if len(l) == 0 {
		return "no faults"
	}
	if len(l) == 1 {
		return l[0].Error()
	}
	return fmt.Sprintf("%d faults; first: %s", len(l), l[0].Error())
}

// Fatal reports whether any fault in the list is fatal.
func (l List) Fatal() bool {
	for i := range l {
		if l[i].Fatal {
			return true
		}
	}
	return false
}

// Count returns how many faults carry the given kind.
func (l List) Count(k Kind) int {
	n := 0
	for i := range l {
		if l[i].Kind == k {
			n++
		}
	}
	return n
}

// FirstOf returns the first fault of the given kind.
func (l List) FirstOf(k Kind) (Fault, bool) {
	for i := range l {
		if l[i].Kind == k {
			return l[i], true
		}
	}
	return Fault{}, false
}

// MinOffset returns the smallest non-negative offset in the list, which is the
// first damaged record in file order. The boolean is false when the list is
// empty or contains no known offsets.
func (l List) MinOffset() (int64, bool) {
	best := int64(-1)
	for i := range l {
		if l[i].Offset < 0 {
			continue
		}
		if best < 0 || l[i].Offset < best {
			best = l[i].Offset
		}
	}
	return best, best >= 0
}

// Sorted returns a copy ordered by file, camera, sequence and offset so JSON
// reports are deterministic across runs and concurrency levels.
func (l List) Sorted() List {
	out := append(List(nil), l...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.File != b.File {
			return a.File < b.File
		}
		if a.Camera != b.Camera {
			return a.Camera < b.Camera
		}
		if a.Seq != b.Seq {
			return a.Seq < b.Seq
		}
		return a.Offset < b.Offset
	})
	return out
}
