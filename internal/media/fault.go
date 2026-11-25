package media

import (
	"errors"
	"fmt"
	"strings"
)

// Kind classifies a located fault. Kinds are stable strings so archived
// reports stay machine-readable across format versions.
type Kind string

const (
	KindTruncated       Kind = "truncated"
	KindBadLength       Kind = "bad_length"
	KindNegativeTS      Kind = "negative_timestamp"
	KindZeroDuration    Kind = "zero_duration"
	KindWrap            Kind = "timestamp_wraparound"
	KindUnsupported     Kind = "unsupported_codec"
	KindMissingIndex    Kind = "missing_index"
	KindCorruptIndex    Kind = "corrupt_index"
	KindIncompleteFrag  Kind = "incomplete_fragment"
	KindDigestMismatch  Kind = "digest_mismatch"
	KindCheckpoint      Kind = "checkpoint"
	KindIO              Kind = "io"
	KindInvalidManifest Kind = "invalid_manifest"
	KindScanLimit       Kind = "scan_limit"
)

// Fault is an error annotated with everything needed to locate it inside the
// source package and to correlate it back to a clip on the timeline.
//
// Offset is a byte offset in the source file (-1 when not applicable) and
// Track is a 1-based track number (0 when the fault is not track-specific).
type Fault struct {
	Kind    Kind   `json:"kind"`
	Segment string `json:"segment"`
	Track   int    `json:"track"`
	Offset  int64  `json:"offset"`
	EditID  string `json:"edit_id,omitempty"`
	Message string `json:"message"`
	// Fatal faults quarantine the owning segment; non-fatal faults are recorded
	// while processing continues (e.g. a truncated tail).
	Fatal bool `json:"fatal"`
}

// NewFault builds a located fault. Pass offset -1 and track 0 when unknown.
func NewFault(kind Kind, segment string, track int, offset int64, format string, args ...any) *Fault {
	return &Fault{
		Kind:    kind,
		Segment: segment,
		Track:   track,
		Offset:  offset,
		Message: fmt.Sprintf(format, args...),
		Fatal:   kind == KindBadLength || kind == KindUnsupported || kind == KindDigestMismatch || kind == KindInvalidManifest,
	}
}

// WithEdit attaches the timeline edit identifier that owns the fault, letting
// a timeline report join demux/index faults back to a clip.
func (f *Fault) WithEdit(editID string) *Fault {
	f.EditID = editID
	return f
}

func (f *Fault) Error() string {
	var b strings.Builder
	b.WriteString(string(f.Kind))
	if f.Segment != "" {
		fmt.Fprintf(&b, " segment=%s", f.Segment)
	}
	if f.Track > 0 {
		fmt.Fprintf(&b, " track=%d", f.Track)
	}
	if f.Offset >= 0 {
		fmt.Fprintf(&b, " offset=%d", f.Offset)
	}
	if f.EditID != "" {
		fmt.Fprintf(&b, " edit=%s", f.EditID)
	}
	b.WriteString(": ")
	b.WriteString(f.Message)
	if f.Fatal {
		b.WriteString(" [fatal]")
	}
	return b.String()
}

// AsFault extracts the first *Fault wrapped inside err, if any.
func AsFault(err error) (*Fault, bool) {
	var f *Fault
	if errors.As(err, &f) {
		return f, true
	}
	return nil, false
}

// IsFatal reports whether err contains a fatal (segment-quarantining) fault.
func IsFatal(err error) bool {
	if f, ok := AsFault(err); ok {
		return f.Fatal
	}
	return false
}

// FaultSet collects located errors while a batch continues processing. It is
// safe for sequential use; pipeline stages run one segment at a time.
type FaultSet struct {
	items []*Fault
}

// Add extracts a *Fault from err and records it. Nil errors are ignored and
// plain errors are wrapped as non-fatal KindIO faults without a location.
func (s *FaultSet) Add(err error) {
	if err == nil {
		return
	}
	if f, ok := AsFault(err); ok {
		s.items = append(s.items, f)
		return
	}
	s.items = append(s.items, NewFault(KindIO, "", 0, -1, "%s", err.Error()))
}

// All returns the recorded faults in arrival order.
func (s *FaultSet) All() []*Fault { return s.items }

// HasFatal reports whether any recorded fault quarantines a segment.
func (s *FaultSet) HasFatal() bool {
	for _, f := range s.items {
		if f.Fatal {
			return true
		}
	}
	return false
}

// FatalCount returns how many fatal faults were recorded.
func (s *FaultSet) FatalCount() int {
	n := 0
	for _, f := range s.items {
		if f.Fatal {
			n++
		}
	}
	return n
}

func (s *FaultSet) Error() string {
	if len(s.items) == 0 {
		return "no faults"
	}
	parts := make([]string, len(s.items))
	for i, f := range s.items {
		parts[i] = f.Error()
	}
	return strings.Join(parts, "; ")
}
