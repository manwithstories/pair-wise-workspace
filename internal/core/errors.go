package core

import "fmt"

// FaultKind classifies every defect the pipeline can detect. Kinds are
// stable strings because they propagate into recovery records and audit
// reports.
type FaultKind string

const (
	FaultTruncatedTail    FaultKind = "truncated_tail"    // input ends mid-structure
	FaultBadCue           FaultKind = "bad_cue"           // isolated malformed subtitle cue
	FaultCorruptMedia     FaultKind = "corrupt_media"     // bitstream damage / truncation
	FaultMissingIndex     FaultKind = "missing_index"     // index entries absent, scan needed
	FaultTimecodeWrap     FaultKind = "timecode_wrap"     // SMPTE crossed 24h boundary
	FaultInvalidTimecode  FaultKind = "invalid_timecode"  // illegal frame number / syntax
	FaultUnrecoverable    FaultKind = "unrecoverable"     // camera cannot be delivered
	FaultManifestMisalign FaultKind = "manifest_misalign" // variant timelines diverge
)

// Fault is the uniform error model: what happened, where (byte offset in
// the source, or -1 when not applicable) and whether the pipeline may
// continue past it.
type Fault struct {
	Kind        FaultKind
	Offset      int64
	Recoverable bool
	Detail      string
}

func (f Fault) Error() string {
	return fmt.Sprintf("%s at offset %d (recoverable=%t): %s",
		f.Kind, f.Offset, f.Recoverable, f.Detail)
}

// NewFault builds a Fault; offset may be -1 when not byte-locatable.
func NewFault(kind FaultKind, offset int64, recoverable bool, detail string) Fault {
	return Fault{Kind: kind, Offset: offset, Recoverable: recoverable, Detail: detail}
}

// Faults is a fault collection with aggregation helpers.
type Faults []Fault

// HasUnrecoverable reports whether any fault blocks delivery of its camera.
func (fs Faults) HasUnrecoverable() bool {
	for _, f := range fs {
		if !f.Recoverable {
			return true
		}
	}
	return false
}

// OfKind returns all faults of one kind.
func (fs Faults) OfKind(k FaultKind) Faults {
	var out Faults
	for _, f := range fs {
		if f.Kind == k {
			out = append(out, f)
		}
	}
	return out
}
