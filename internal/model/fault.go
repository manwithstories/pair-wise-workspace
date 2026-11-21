package model

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// FaultKind classifies every failure the pipeline can report. The same enum
// is produced by manifest parsing, scanning, stream validation, RTP analysis
// and recovery so a conclusion can always be traced to one category.
type FaultKind string

const (
	// FaultManifest marks a structurally invalid or unsupported manifest.
	FaultManifest FaultKind = "manifest_invalid"
	// FaultMissingSegment marks a manifest reference whose media is absent.
	FaultMissingSegment FaultKind = "missing_segment"
	// FaultTruncatedBox marks an MP4 box whose declared size exceeds the file.
	FaultTruncatedBox FaultKind = "truncated_box"
	// FaultBoxBounds marks an MP4 box with an impossible size.
	FaultBoxBounds FaultKind = "box_bounds"
	// FaultBoxTooLarge marks a well-formed but unreasonably large box.
	FaultBoxTooLarge FaultKind = "box_too_large"
	// FaultTimestamp marks a non-monotonic, negative or wrapped track clock.
	FaultTimestamp FaultKind = "timestamp"
	// FaultKeyframe marks an out-of-spec keyframe (GOP) interval.
	FaultKeyframe FaultKind = "keyframe_interval"
	// FaultAVSync marks a decoded audio/video presentation skew.
	FaultAVSync FaultKind = "av_sync"
	// FaultPacketLoss marks unrecovered RTP sequence loss.
	FaultPacketLoss FaultKind = "packet_loss"
	// FaultRTPScale marks RTP timestamp clock discontinuities.
	FaultRTPScale FaultKind = "rtp_timestamp"
)

// AllFaultKinds lists every kind in a stable order for reports and tests.
var AllFaultKinds = []FaultKind{
	FaultManifest, FaultMissingSegment, FaultTruncatedBox, FaultBoxBounds,
	FaultBoxTooLarge, FaultTimestamp, FaultKeyframe, FaultAVSync, FaultPacketLoss, FaultRTPScale,
}

// Valid reports whether k is a known fault kind.
func (k FaultKind) Valid() bool {
	for _, other := range AllFaultKinds {
		if k == other {
			return true
		}
	}
	return false
}

// Severity expresses how much a fault affects a viewer.
type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityWarning  Severity = "warning"
	SeverityCritical Severity = "critical"
)

// Sentinel errors returned by stream-level validation.
var (
	ErrTruncated   = errors.New("model: truncated box")
	ErrBoxBounds   = errors.New("model: invalid box bounds")
	ErrBoxTooLarge = errors.New("model: box too large")
)

// ErrNotFound is returned by lookups that had no matching fault.
var ErrNotFound = errors.New("model: fault not found")

// Fault is the single error model shared across subsystems. Ranges are
// expressed on the shared rational clock; byte ranges are inclusive.
type Fault struct {
	Kind       FaultKind `json:"kind"`
	Severity   Severity  `json:"severity"`
	Stream     string    `json:"stream,omitempty"`
	Segment    string    `json:"segment,omitempty"`
	Message    string    `json:"message"`
	ByteStart  int64     `json:"byte_start"`
	ByteEnd    int64     `json:"byte_end"`
	TimeStart  Rational  `json:"time_start"`
	TimeEnd    Rational  `json:"time_end"`
	FirstBad   bool      `json:"first_bad,omitempty"`
	IndexEntry int       `json:"index_entry"`
}

// Faults is an ordered collection that keeps the first occurrence of each
// (kind,segment) pair while still allowing duplicates to be counted.
type Faults struct {
	items []Fault
}

// Add appends f.
func (f *Faults) Add(fault Fault) { f.items = append(f.items, fault) }

// Len returns the number of recorded faults.
func (f *Faults) Len() int { return len(f.items) }

// All returns a copy of the recorded faults.
func (f *Faults) All() []Fault {
	out := make([]Fault, len(f.items))
	copy(out, f.items)
	return out
}

// First returns the earliest fault of the given kind by time, then by index
// entry. This is how the pipeline identifies the first missing dependency or
// the first undecodable range.
func (f *Faults) First(kind FaultKind) (Fault, error) {
	var best *Fault
	for i := range f.items {
		fi := &f.items[i]
		if fi.Kind != kind {
			continue
		}
		if best == nil || fi.TimeStart.Less(best.TimeStart) ||
			(fi.TimeStart.Equal(best.TimeStart) && fi.IndexEntry < best.IndexEntry) {
			best = fi
		}
	}
	if best == nil {
		return Fault{}, fmt.Errorf("%w: %s", ErrNotFound, kind)
	}
	return *best, nil
}

// ByKind returns every fault of the given kind in insertion order.
func (f *Faults) ByKind(kind FaultKind) []Fault {
	var out []Fault
	for _, fi := range f.items {
		if fi.Kind == kind {
			out = append(out, fi)
		}
	}
	return out
}

// Counts returns the number of faults per kind.
func (f *Faults) Counts() map[FaultKind]int {
	out := make(map[FaultKind]int, len(f.items))
	for _, fi := range f.items {
		out[fi.Kind]++
	}
	return out
}

// Has reports whether any fault of kind was recorded.
func (f *Faults) Has(kind FaultKind) bool {
	for _, fi := range f.items {
		if fi.Kind == kind {
			return true
		}
	}
	return false
}

// Sort orders faults by time, then segment, then kind, deterministically.
func (f *Faults) Sort() {
	sort.SliceStable(f.items, func(i, j int) bool {
		a, b := f.items[i], f.items[j]
		if c := a.TimeStart.Cmp(b.TimeStart); c != 0 {
			return c < 0
		}
		if a.Segment != b.Segment {
			return a.Segment < b.Segment
		}
		return a.Kind < b.Kind
	})
}

// Validate checks structural invariants of a fault.
func (f Fault) Validate() error {
	if !f.Kind.Valid() {
		return fmt.Errorf("model: unknown fault kind %q", f.Kind)
	}
	switch f.Severity {
	case SeverityInfo, SeverityWarning, SeverityCritical:
	default:
		return fmt.Errorf("model: bad severity %q", f.Severity)
	}
	if f.ByteStart < 0 || f.ByteEnd < f.ByteStart {
		return fmt.Errorf("model: bad byte range %d-%d", f.ByteStart, f.ByteEnd)
	}
	if f.TimeEnd.Less(f.TimeStart) {
		return fmt.Errorf("model: bad time range %s-%s", f.TimeStart, f.TimeEnd)
	}
	return nil
}

// Summary renders a compact one-line description for terminal output.
func (f Fault) Summary() string {
	var b strings.Builder
	b.WriteString(string(f.Kind))
	if f.Segment != "" {
		b.WriteString(" segment=")
		b.WriteString(f.Segment)
	}
	if f.Stream != "" {
		b.WriteString(" stream=")
		b.WriteString(f.Stream)
	}
	fmt.Fprintf(&b, " bytes=%d..%d", f.ByteStart, f.ByteEnd)
	if ms, err := f.TimeStart.MillisRounded(); err == nil {
		fmt.Fprintf(&b, " t=%dms", ms)
	}
	b.WriteString(" ")
	b.WriteString(f.Message)
	return b.String()
}
