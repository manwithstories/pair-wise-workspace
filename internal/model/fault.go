// Package model defines the domain types shared across the fastpost
// pipeline and the structured fault reporting used by every stage.
package model

import "strings"

// FaultKind enumerates the reportable input conditions. Values are stable
// strings because they appear in terminal output and checksummed reports.
type FaultKind string

const (
	// FaultNegativeDuration is emitted when a duration or timestamp is
	// negative where only non-negative values are legal.
	FaultNegativeDuration FaultKind = "negative-duration"
	// FaultOverflow is emitted when a timebase conversion cannot fit the
	// result into the common 90 kHz tick range.
	FaultOverflow FaultKind = "overflow"
	// FaultUnmappableEvent is emitted when a timed event cannot be placed
	// on the unified timeline (for example it lies outside the track window).
	FaultUnmappableEvent FaultKind = "unmappable-event"
	// FaultSEITruncated is emitted when an SEI NAL unit ends prematurely.
	FaultSEITruncated FaultKind = "sei-truncated"
	// FaultBoxTruncated is emitted when an ID3/emsg/container box is short.
	FaultBoxTruncated FaultKind = "box-truncated"
	// FaultChannelMapping is emitted when an audio track carries a channel
	// layout the remuxer is not allowed to represent.
	FaultChannelMapping FaultKind = "illegal-channel-mapping"
	// FaultColorConflict is emitted when color metadata cannot be reconciled.
	FaultColorConflict FaultKind = "color-conflict"
	// FaultGraphCycle is emitted when the track dependency graph has a cycle.
	FaultGraphCycle FaultKind = "graph-cycle"
	// FaultChecksum is emitted when a re-run produces a different manifest.
	FaultChecksum FaultKind = "checksum-mismatch"
)

// Fault is a single structured input problem. Offset is the byte offset
// inside the source blob of Track; -1 means the offset is not applicable.
type Fault struct {
	Kind   FaultKind
	Track  string
	Offset int64
	Reason string
}

// Error makes Fault usable as an error while preserving structured fields.
func (f Fault) Error() string { return f.String() }

// String renders a fault as a stable single-line diagnostic.
func (f Fault) String() string {
	if f.Offset >= 0 {
		return "fault=" + string(f.Kind) + " track=" + f.Track +
			" offset=" + itoa(f.Offset) + " reason=" + f.Reason
	}
	return "fault=" + string(f.Kind) + " track=" + f.Track + " reason=" + f.Reason
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// InputError aggregates the faults that make an input unusable. The CLI
// maps this error class to exit code 2.
type InputError struct {
	Faults []Fault
}

// Error implements the error interface.
func (e *InputError) Error() string {
	parts := make([]string, len(e.Faults))
	for i, f := range e.Faults {
		parts[i] = f.String()
	}
	return "unrecoverable input error(s): " + strings.Join(parts, "; ")
}

// Collector gathers faults in pipeline order so that a single run can
// report every problem found instead of stopping at the first one.
type Collector struct {
	faults []Fault
}

// Add records a fault.
func (c *Collector) Add(kind FaultKind, track string, offset int64, reason string) {
	c.faults = append(c.faults, Fault{Kind: kind, Track: track, Offset: offset, Reason: reason})
}

// AddFault records an already constructed fault.
func (c *Collector) AddFault(f Fault) {
	c.faults = append(c.faults, f)
}

// All returns the recorded faults in insertion order.
func (c *Collector) All() []Fault {
	return c.faults
}

// Has reports whether any fault was collected.
func (c *Collector) Has() bool { return len(c.faults) > 0 }

// Err returns an *InputError when faults exist, otherwise nil.
func (c *Collector) Err() error {
	if !c.Has() {
		return nil
	}
	return &InputError{Faults: append([]Fault(nil), c.faults...)}
}
