// Package fault defines the cross-module error vocabulary.  Every subsystem
// reports damage as a Fault carrying the module that detected it, the affected
// source byte offset when known, the track when known and whether processing
// can continue.  The archivecheck command uses Kind and ExitCode to choose the
// process exit status.
package fault

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Module identifies the subsystem that detected a fault.
type Module string

const (
	ModuleEBML      Module = "ebml"
	ModuleTransport Module = "transport"
	ModuleTimeline  Module = "timeline"
	ModuleIndex     Module = "index"
	ModuleManifest  Module = "manifest"
	ModuleJournal   Module = "journal"
	ModuleTxn       Module = "txn"
	ModuleVerify    Module = "verify"
)

// Kind classifies damage.  The set is deliberately small so that reports stay
// comparable across runs.
type Kind string

const (
	KindTruncated        Kind = "truncated-element"
	KindBadElement       Kind = "malformed-element"
	KindUnsizedElement   Kind = "unknown-size-element"
	KindTimestampRegress Kind = "timestamp-regression"
	KindTimestampRepeat  Kind = "timestamp-duplicate"
	KindDrift            Kind = "cross-track-drift"
	KindPacketGap        Kind = "packet-gap"
	KindPacketDuplicate  Kind = "packet-duplicate"
	KindPacketReorder    Kind = "packet-reorder"
	KindBandwidthSpike   Kind = "bandwidth-spike"
	KindBudgetExceeded   Kind = "memory-budget-exceeded"
	KindManifestDrift    Kind = "manifest-alignment"
	KindJournalTail      Kind = "journal-tail-truncated"
	KindJournalRecord    Kind = "journal-corrupt-record"
	KindInterrupted      Kind = "stage-interrupted"
	KindVerification     Kind = "verification-failed"
	KindIndexMisaligned  Kind = "index-misaligned"
)

// Offset is a source byte position.  Valid distinguishes a real offset 0 from
// "unknown", which matters when a truncated first element sits at byte zero.
type Offset struct {
	Value uint64
	Valid bool
}

// NewOffset returns a valid offset.
func NewOffset(v uint64) Offset { return Offset{Value: v, Valid: true} }

// NoOffset returns the unknown-offset sentinel.
func NoOffset() Offset { return Offset{} }

// String renders the offset or "?" when unknown.
func (o Offset) String() string {
	if !o.Valid {
		return "?"
	}
	return fmt.Sprintf("0x%x", o.Value)
}

// Fault is a single diagnosable defect.
type Fault struct {
	Module      Module `json:"module"`
	Kind        Kind   `json:"kind"`
	Offset      Offset `json:"-"`
	OffsetValue uint64 `json:"offset,omitempty"`
	HasOffset   bool   `json:"offset_known"`
	Track       uint64 `json:"track,omitempty"`
	HasTrack    bool   `json:"track_known"`
	Message     string `json:"message"`
	// Recoverable means the pipeline produced a usable, explicitly marked
	// result rather than silently dropping data.
	Recoverable bool `json:"recoverable"`
}

// New builds a recoverable fault.
func New(m Module, k Kind, off Offset, msg string) *Fault {
	return &Fault{Module: m, Kind: k, Offset: off, HasOffset: off.Valid, OffsetValue: off.Value, Message: msg, Recoverable: true}
}

// OnTrack sets the track association and returns the fault for chaining.
func (f *Fault) OnTrack(track uint64) *Fault {
	f.Track = track
	f.HasTrack = true
	return f
}

// Fatal marks the fault as non-recoverable.
func (f *Fault) Fatal() *Fault {
	f.Recoverable = false
	return f
}

func (f *Fault) Error() string {
	var b strings.Builder
	b.WriteString(string(f.Module))
	b.WriteString(": ")
	b.WriteString(string(f.Kind))
	b.WriteString(" at ")
	b.WriteString(f.Offset.String())
	if f.HasTrack {
		fmt.Fprintf(&b, " track=%d", f.Track)
	}
	if f.Message != "" {
		b.WriteString(": ")
		b.WriteString(f.Message)
	}
	if !f.Recoverable {
		b.WriteString(" [fatal]")
	}
	return b.String()
}

// Set is an ordered collection of faults.  It implements error so callers can
// return the whole diagnosed set.
type Set struct {
	faults []*Fault
}

// Add appends a fault, ignoring nil.
func (s *Set) Add(f *Fault) {
	if f != nil {
		s.faults = append(s.faults, f)
	}
}

// AddAll appends every fault from other.
func (s *Set) AddAll(other *Set) {
	if other != nil {
		s.faults = append(s.faults, other.faults...)
	}
}

// Len returns the number of faults.
func (s *Set) Len() int { return len(s.faults) }

// Faults returns a copy of the fault slice sorted by offset then kind.
func (s *Set) Faults() []*Fault {
	out := make([]*Fault, len(s.faults))
	copy(out, s.faults)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Offset.Valid != out[j].Offset.Valid {
			return out[i].Offset.Valid
		}
		if out[i].Offset.Value != out[j].Offset.Value {
			return out[i].Offset.Value < out[j].Offset.Value
		}
		return out[i].Kind < out[j].Kind
	})
	return out
}

// Count returns how many faults have the given kind.
func (s *Set) Count(k Kind) int {
	n := 0
	for _, f := range s.faults {
		if f.Kind == k {
			n++
		}
	}
	return n
}

// Kinds returns the distinct kinds present, sorted.
func (s *Set) Kinds() []Kind {
	seen := make(map[Kind]bool, len(s.faults))
	out := make([]Kind, 0, len(s.faults))
	for _, f := range s.faults {
		if !seen[f.Kind] {
			seen[f.Kind] = true
			out = append(out, f.Kind)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// HasFatal reports whether any fault is non-recoverable.
func (s *Set) HasFatal() bool {
	for _, f := range s.faults {
		if !f.Recoverable {
			return true
		}
	}
	return false
}

// Err returns nil for an empty set, otherwise itself.
func (s *Set) Err() error {
	if s == nil || len(s.faults) == 0 {
		return nil
	}
	return s
}

// Error renders the first fault plus a count of the remainder.
func (s *Set) Error() string {
	if len(s.faults) == 0 {
		return "no faults"
	}
	first := s.Faults()[0]
	if len(s.faults) == 1 {
		return first.Error()
	}
	return fmt.Sprintf("%s (and %d more)", first.Error(), len(s.faults)-1)
}

// First returns the fault that sorts first, or nil.
func (s *Set) First() *Fault {
	if len(s.faults) == 0 {
		return nil
	}
	return s.Faults()[0]
}

// Exit codes shared with the command layer.
const (
	ExitOK        = 0
	ExitDamaged   = 2 // input damaged but fully diagnosed
	ExitIntegrity = 3 // transaction recovery or integrity check failed
)

// ExitCode maps the fault set to the process exit status: fatal faults and
// integrity/transaction failures are 3, recoverable damage is 2, clean is 0.
func (s *Set) ExitCode() int {
	if s == nil || len(s.faults) == 0 {
		return ExitOK
	}
	for _, f := range s.faults {
		if !f.Recoverable || f.Kind == KindVerification || f.Kind == KindJournalRecord {
			return ExitIntegrity
		}
	}
	return ExitDamaged
}

// Is works with errors.Is for sentinel kinds.
var (
	ErrTruncated = errors.New("truncated")
)
