package model

import (
	"errors"
	"fmt"
)

// Phase identifies the pipeline stage that produced a fault. Keeping the
// phase on every record lets an operator locate a degradation without
// cross-referencing logs from several tools.
type Phase string

const (
	PhaseManifest Phase = "manifest"
	PhaseRTP      Phase = "rtp"
	PhaseRepair   Phase = "repair"
	PhaseBuffer   Phase = "buffer"
	PhaseSync     Phase = "syncer"
	PhaseBitrate  Phase = "bitrate"
	PhasePipeline Phase = "pipeline"
)

// FaultKind classifies a record so reports can count each failure mode.
type FaultKind string

const (
	FaultPathRejected      FaultKind = "path_rejected"
	FaultDuplicateSegment  FaultKind = "duplicate_segment"
	FaultInvalidDuration   FaultKind = "invalid_duration"
	FaultDuplicatePacket   FaultKind = "duplicate_packet"
	FaultLatePacket        FaultKind = "late_packet"
	FaultReorder           FaultKind = "reordered"
	FaultLoss              FaultKind = "loss"
	FaultRetransmitted     FaultKind = "retransmitted"
	FaultFECRecovered      FaultKind = "fec_recovered"
	FaultUnrecoverable     FaultKind = "unrecoverable"
	FaultCRC               FaultKind = "crc_mismatch"
	FaultBackpressure      FaultKind = "backpressure"
	FaultOversize          FaultKind = "oversize_input"
	FaultDiscontinuity     FaultKind = "discontinuity"
	FaultStrictUnrecovered FaultKind = "strict_unrecovered"
)

// Fault is one structured degradation record. SeqLo/SeqHi describe the
// inclusive RTP sequence range the fault covers; a single-packet fault has
// equal bounds. Segment names the playlist segment when the fault maps to
// one, otherwise it is empty.
type Fault struct {
	Phase   Phase
	Kind    FaultKind
	SeqLo   int64
	SeqHi   int64
	Segment string
	Detail  string
}

// String renders a fault for terminal output.
func (f Fault) String() string {
	span := fmt.Sprintf("seq[%d..%d]", f.SeqLo, f.SeqHi)
	if f.SeqLo == f.SeqHi {
		span = fmt.Sprintf("seq[%d]", f.SeqLo)
	}
	if f.Segment != "" {
		span += " seg=" + f.Segment
	}
	if f.Detail != "" {
		return fmt.Sprintf("%s/%s %s: %s", f.Phase, f.Kind, span, f.Detail)
	}
	return fmt.Sprintf("%s/%s %s", f.Phase, f.Kind, span)
}

// Gap is an explicit, unrepaired hole in the recovered timeline. Gaps are
// never silently concatenated: downstream stages render a discontinuity and
// report the hole so no pseudo-complete segment is produced.
type Gap struct {
	Segment string
	SeqLo   int64
	SeqHi   int64
	Start   Nano
	End     Nano
	Reason  FaultKind
}

// Length reports how many sequence numbers the gap covers.
func (g Gap) Length() int64 { return g.SeqHi - g.SeqLo + 1 }

// FaultLog collects every fault and explicit gap in encounter order. It is
// safe for single-goroutine pipeline use; stages share one instance.
type FaultLog struct {
	faults []Fault
	gaps   []Gap
}

// NewFaultLog returns an empty log.
func NewFaultLog() *FaultLog { return &FaultLog{} }

// Add records a fault.
func (l *FaultLog) Add(f Fault) { l.faults = append(l.faults, f) }

// AddGap records an explicit timeline gap and, for consistency, a matching
// unrecoverable fault carrying the same packet range.
func (l *FaultLog) AddGap(g Gap) {
	l.gaps = append(l.gaps, g)
	l.faults = append(l.faults, Fault{
		Phase:   PhaseRepair,
		Kind:    g.Reason,
		SeqLo:   g.SeqLo,
		SeqHi:   g.SeqHi,
		Segment: g.Segment,
		Detail:  fmt.Sprintf("gap %d..%d on [%d,%d)", g.SeqLo, g.SeqHi, g.Start, g.End),
	})
}

// Faults returns the accumulated faults in encounter order.
func (l *FaultLog) Faults() []Fault {
	out := make([]Fault, len(l.faults))
	copy(out, l.faults)
	return out
}

// Gaps returns the explicit gaps in encounter order.
func (l *FaultLog) Gaps() []Gap {
	out := make([]Gap, len(l.gaps))
	copy(out, l.gaps)
	return out
}

// Count returns how many faults of the given kind were recorded.
func (l *FaultLog) Count(kind FaultKind) int {
	n := 0
	for _, f := range l.faults {
		if f.Kind == kind {
			n++
		}
	}
	return n
}

// Sentinel errors for invariant failures and strict-mode aborts.
var (
	// ErrInvariant is returned when a bounded resource or a monotonicity
	// invariant is violated; CLI maps it to exit code 1.
	ErrInvariant = errors.New("invariant violation")
	// ErrStrictUnrecovered is returned when strict recovery mode meets a
	// segment that cannot be fully rebuilt; CLI maps it to exit code 2.
	ErrStrictUnrecovered = errors.New("strict mode: unrecoverable segment")
)

// InvariantError wraps ErrInvariant with a phase-specific message.
func InvariantError(phase Phase, format string, args ...any) error {
	return fmt.Errorf("%w: %s: %s", ErrInvariant, phase, fmt.Sprintf(format, args...))
}
