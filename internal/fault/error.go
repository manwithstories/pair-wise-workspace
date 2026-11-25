// Package fault defines the unified damage record carried by the
// manifest, scanner, repair and audit subsystems. Every record names a
// track, a byte/sample offset where the damage was observed and a
// recoverability level so callers never have to parse error strings.
package fault

import (
	"errors"
	"fmt"
	"strings"
)

// Level classifies how the pipeline may react to a Fault.
type Level int

const (
	// LevelRecoverable means redundancy or deterministic rules restore
	// the timeline/sample (e.g. single RTP loss covered by XOR).
	LevelRecoverable Level = iota + 1
	// LevelAmbiguous means evidence conflicts; the affected range is
	// isolated and timestamps are never guessed.
	LevelAmbiguous
	// LevelFatal means structural damage prevents trustworthy repair;
	// the affected artifact is quarantined explicitly.
	LevelFatal
)

// String renders the stable machine-readable level name.
func (l Level) String() string {
	switch l {
	case LevelRecoverable:
		return "recoverable"
	case LevelAmbiguous:
		return "ambiguous"
	case LevelFatal:
		return "fatal"
	default:
		return fmt.Sprintf("level(%d)", int(l))
	}
}

// Fault is one localized damage observation.
type Fault struct {
	// Code is a stable snake_case identifier (e.g. "trun_truncated").
	Code string `json:"code"`
	// Track is the track/representation id; empty when global.
	Track string `json:"track,omitempty"`
	// Offset is the byte offset (container damage) or sequence/sample
	// offset (timeline/RTP damage) the record refers to.
	Offset int64 `json:"offset"`
	// Level states whether repair is possible.
	Level Level `json:"level"`
	// Detail is a human-readable, non-sensitive explanation.
	Detail string `json:"detail,omitempty"`
}

// New builds a Fault with the required localization triple.
func New(code, track string, offset int64, level Level, detail string) Fault {
	return Fault{Code: code, Track: track, Offset: offset, Level: level, Detail: detail}
}

func (f Fault) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s at offset %d [%s]", f.Code, f.Offset, f.Level)
	if f.Track != "" {
		fmt.Fprintf(&b, " track=%s", f.Track)
	}
	if f.Detail != "" {
		fmt.Fprintf(&b, ": %s", f.Detail)
	}
	return b.String()
}

// Record is the aggregate error type: subsystems collect every Fault
// instead of aborting at the first damaged location.
type Record struct {
	Faults []Fault `json:"faults"`
}

// Error implements error.
func (r *Record) Error() string {
	if r == nil || len(r.Faults) == 0 {
		return "fault: empty record"
	}
	parts := make([]string, 0, len(r.Faults))
	for _, f := range r.Faults {
		parts = append(parts, f.Error())
	}
	return strings.Join(parts, "; ")
}

// NewRecord creates an empty fault record.
func NewRecord() *Record { return &Record{} }

// Add appends a fault and returns the same record for chaining.
func (r *Record) Add(f Fault) *Record {
	r.Faults = append(r.Faults, f)
	return r
}

// HasFatal reports whether any collected fault is LevelFatal.
func (r *Record) HasFatal() bool {
	if r == nil {
		return false
	}
	for _, f := range r.Faults {
		if f.Level == LevelFatal {
			return true
		}
	}
	return false
}

// OfLevel returns the faults at exactly the requested level.
func (r *Record) OfLevel(level Level) []Fault {
	if r == nil {
		return nil
	}
	var out []Fault
	for _, f := range r.Faults {
		if f.Level == level {
			out = append(out, f)
		}
	}
	return out
}

// AsRecord extracts a *Record from an error chain.
func AsRecord(err error) (*Record, bool) {
	var rec *Record
	if errors.As(err, &rec) {
		return rec, true
	}
	return nil, false
}

// ExitCode maps faults to the CLI contract: 3 for unrecoverable
// structural damage, 0 otherwise (verification failures are handled by
// the audit package and map to 4 there).
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	if rec, ok := AsRecord(err); ok && rec.HasFatal() {
		return 3
	}
	return 4
}
