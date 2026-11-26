// Package clock provides the shared timebase and fault model used by the
// demux, mapping and remux subsystems of the relay checker.
package clock

import "fmt"

// FaultKind classifies stream faults detected while relaying segments.
type FaultKind string

const (
	// FaultGap is an mfhd sequence-number gap. Missing numbers must never
	// be silently filled with fabricated samples.
	FaultGap FaultKind = "gap"
	// FaultRollback is a tfdt base-decode-time rollback.
	FaultRollback FaultKind = "rollback"
	// FaultDuplicate is a segment whose content digest was already seen.
	FaultDuplicate FaultKind = "duplicate"
	// FaultTruncate is a truncated box or file.
	FaultTruncate FaultKind = "truncate"
	// FaultLoss is a segment that is missing entirely.
	FaultLoss FaultKind = "loss"
	// FaultMapConflict is a track reference that cannot be mapped.
	FaultMapConflict FaultKind = "map-conflict"
	// FaultBoxBound is a box that is out of bounds, nested too deep or
	// larger than the configured limit.
	FaultBoxBound FaultKind = "box-bound"
	// FaultSampleTable is a sample table exceeding configured limits.
	FaultSampleTable FaultKind = "sample-table"
	// FaultUndetermined marks a segment whose decode time cannot be
	// determined; it must be quarantined with a reason.
	FaultUndetermined FaultKind = "undetermined"
)

// Fault describes one detected stream fault.
type Fault struct {
	Kind   FaultKind `json:"kind"`
	Seq    uint32    `json:"seq,omitempty"`    // segment sequence, 0 if n/a
	Track  uint32    `json:"track,omitempty"`  // track id, 0 if n/a
	Offset int64     `json:"offset,omitempty"` // first error offset, -1 if n/a
	Detail string    `json:"detail,omitempty"`
}

// Error implements error.
func (f Fault) Error() string {
	s := string(f.Kind)
	if f.Seq != 0 {
		s += fmt.Sprintf(" seq=%d", f.Seq)
	}
	if f.Track != 0 {
		s += fmt.Sprintf(" track=%d", f.Track)
	}
	if f.Offset >= 0 {
		s += fmt.Sprintf(" offset=%d", f.Offset)
	}
	if f.Detail != "" {
		s += ": " + f.Detail
	}
	return s
}

// NewFault builds a fault with the offset field defaulted to -1 (n/a).
func NewFault(kind FaultKind, seq, track uint32, offset int64, detail string) Fault {
	return Fault{Kind: kind, Seq: seq, Track: track, Offset: offset, Detail: detail}
}
