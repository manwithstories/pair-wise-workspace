package model

import "fmt"

// FaultClass is the coarse category of a defect. Classes are stable strings
// used in JSON manifests and checkpoint records; do not renumber them.
type FaultClass string

const (
	// FaultChecksum: stored SHA-256 does not match the frame payload.
	FaultChecksum FaultClass = "checksum_mismatch"
	// FaultDuplicate: two records carry the same sequence number.
	FaultDuplicate FaultClass = "duplicate_frame"
	// FaultBackwards: a device timestamp moves backwards without an RTP
	// rollover explanation (device clock reset / jump point error).
	FaultBackwards FaultClass = "backwards_timestamp"
	// FaultMissing: a sequence gap between adjacent frames.
	FaultMissing FaultClass = "missing_frame"
	// FaultRollover: a 32-bit timestamp rollover was detected and unwrapped.
	FaultRollover FaultClass = "timestamp_rollover"
	// FaultAudioLayout: unknown channel layout or invalid sample rate.
	FaultAudioLayout FaultClass = "audio_layout_conflict"
	// FaultAudioBoundary: a track duration is not divisible by its frame size.
	FaultAudioBoundary FaultClass = "audio_boundary"
	// FaultColorConflict: two sources assert different color metadata.
	FaultColorConflict FaultClass = "color_conflict"
	// FaultColorMissing: color metadata absent and no project declaration.
	FaultColorMissing FaultClass = "color_missing"
	// FaultGraphCycle: the track dependency graph contains a cycle.
	FaultGraphCycle FaultClass = "graph_cycle"
	// FaultIndexBounds: an index entry points outside the source frame range.
	FaultIndexBounds FaultClass = "index_out_of_bounds"
	// FaultIndexMonotonic: indexed timestamps are not strictly increasing.
	FaultIndexMonotonic FaultClass = "index_non_monotonic"
	// FaultCheckpoint: a checkpoint cannot be reused (digest mismatch).
	FaultCheckpoint FaultClass = "checkpoint_mismatch"
)

// Severity ranks faults for ordering and exit-code selection.
type Severity int

const (
	SeverityInfo        Severity = iota // repaired automatically, evidence kept
	SeverityRecoverable                 // repairable under an explicit policy
	SeverityFatal                       // pipeline must stop the affected branch
)

// Recoverable mirrors severity as the JSON-facing boolean pair.
func (s Severity) Recoverable() bool { return s != SeverityFatal }

// SourceLocation pinpoints a defect inside an input file.
type SourceLocation struct {
	File   string `json:"file"`
	Track  string `json:"track_id"`
	Seq    uint64 `json:"frame_seq"`
	Offset int64  `json:"byte_offset"` // offset of the record line in File
}

// String gives a grep-friendly location.
func (l SourceLocation) String() string {
	return fmt.Sprintf("%s#track=%s#seq=%d@offset=%d", l.File, l.Track, l.Seq, l.Offset)
}

// Fault is the uniform defect record shared by every module.
type Fault struct {
	Class       FaultClass     `json:"class"`
	Severity    Severity       `json:"severity"`
	Location    SourceLocation `json:"location"`
	Message     string         `json:"message"`
	Expected    string         `json:"expected,omitempty"`
	Actual      string         `json:"actual,omitempty"`
	Inferred    bool           `json:"inferred"`      // value was inferred, not read
	SourceCheck string         `json:"source_sha256"` // never rewritten
}

// Recoverable reports whether the pipeline can continue past the fault.
func (f Fault) Recoverable() bool { return f.Severity.Recoverable() }

func (f Fault) Error() string {
	return fmt.Sprintf("%s: %s (%s)", f.Class, f.Message, f.Location)
}

// FaultError wraps a fault list as an error so stages can return it through
// ordinary error channels while preserving structured records.
type FaultError struct{ Faults []Fault }

func (e *FaultError) Error() string {
	if len(e.Faults) == 1 {
		return e.Faults[0].Error()
	}
	return fmt.Sprintf("%d faults, first: %s", len(e.Faults), e.Faults[0].Error())
}

// HasFatal reports whether any fault in the list stops the pipeline.
func HasFatal(faults []Fault) bool {
	for i := range faults {
		if faults[i].Severity == SeverityFatal {
			return true
		}
	}
	return false
}

// ExitCode maps a fault set onto the documented process exit codes:
//
//	0 success, 2 corrupt input / verification failure,
//	3 dependency or graph error, 4 resume-state mismatch.
func ExitCode(faults []Fault) int {
	code := 0
	for i := range faults {
		var c int
		switch faults[i].Class {
		case FaultGraphCycle:
			c = 3
		case FaultCheckpoint:
			c = 4
		default:
			c = 2
		}
		if c > code {
			code = c
		}
	}
	return code
}
