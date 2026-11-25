package model

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Kind classifies a fault so the CLI can map it to an exit code and the
// reporters can group damage without parsing messages.
type Kind int

const (
	// KindInvalid is a bad parameter or structurally unusable asset (exit 2).
	KindInvalid Kind = iota
	// KindTruncated is a short tail: only the trailing residual block is bad.
	KindTruncated
	// KindCRC marks a frame range whose CRC32C check failed.
	KindCRC
	// KindAlignment marks padding/frame-alignment violations.
	KindAlignment
	// KindClockUndecidable marks an RTP observation past half a wrap period.
	KindClockUndecidable
	// KindIndex marks an index that cannot be validated or recovered (exit 3).
	KindIndex
	// KindInterrupted is a checkpoint-driven, intentional stop (exit 75).
	KindInterrupted
)

// Sentinel errors used for errors.Is based exit-code selection.
var (
	ErrInvalid            = errors.New("invalid parameter or asset")
	ErrIndexUnrecoverable = errors.New("index unrecoverable")
	ErrInterrupted        = errors.New("interrupted after checkpoint")
)

// Fault is a single located problem. ByteOffset is the source byte offset of
// the first affected byte, or -1 when the fault has no byte position.
type Fault struct {
	Kind       Kind       `json:"kind"`
	Track      string     `json:"track"`
	Range      FrameRange `json:"range"`
	ByteOffset int64      `json:"byte_offset"`
	Message    string     `json:"message"`
}

// Error implements error.
func (f Fault) Error() string {
	off := "n/a"
	if f.ByteOffset >= 0 {
		off = itoa(f.ByteOffset)
	}
	return fmt.Sprintf("%s: track=%s frames=[%d,%d) byte=%s: %s",
		f.Kind, f.Track, f.Range.First, f.Range.Last, off, f.Message)
}

// FaultError wraps a Fault and anchors it to a sentinel for errors.Is.
type FaultError struct {
	Fault     Fault
	sentinel  error
}

// Error delegates to the embedded Fault.
func (e *FaultError) Error() string { return e.Fault.Error() }

// Unwrap exposes the anchored sentinel.
func (e *FaultError) Unwrap() error { return e.sentinel }

// NewFault builds a located error anchored to the sentinel for its kind.
func NewFault(k Kind, track string, r FrameRange, byteOffset int64, msg string) *FaultError {
	return &FaultError{
		Fault:    Fault{Kind: k, Track: track, Range: r, ByteOffset: byteOffset, Message: msg},
		sentinel: sentinelFor(k),
	}
}

func sentinelFor(k Kind) error {
	switch k {
	case KindIndex:
		return ErrIndexUnrecoverable
	case KindInterrupted:
		return ErrInterrupted
	default:
		return ErrInvalid
	}
}

// String gives a stable, human-readable label.
func (k Kind) String() string {
	switch k {
	case KindInvalid:
		return "invalid"
	case KindTruncated:
		return "truncated"
	case KindCRC:
		return "crc"
	case KindAlignment:
		return "alignment"
	case KindClockUndecidable:
		return "clock-undecidable"
	case KindIndex:
		return "index"
	case KindInterrupted:
		return "interrupted"
	default:
		return "unknown"
	}
}

// MarshalJSON emits the stable string label rather than the numeric enum.
func (k Kind) MarshalJSON() ([]byte, error) {
	return json.Marshal(k.String())
}

// MarshalJSON renders a frame range as a compact [first, last] array.
func (fr FrameRange) MarshalJSON() ([]byte, error) {
	return json.Marshal([2]int64{int64(fr.First), int64(fr.Last)})
}

// FaultSet is an ordered, de-duplicated collection of located faults. Order of
// first insertion is preserved so reports are deterministic.
type FaultSet struct {
	items []Fault
	seen  map[Fault]struct{}
}

// NewFaultSet returns an empty set.
func NewFaultSet() *FaultSet {
	return &FaultSet{seen: make(map[Fault]struct{})}
}

// Add inserts a fault unless an identical one is already present.
func (s *FaultSet) Add(f Fault) {
	if s.seen == nil {
		s.seen = make(map[Fault]struct{})
	}
	if _, ok := s.seen[f]; ok {
		return
	}
	s.seen[f] = struct{}{}
	s.items = append(s.items, f)
}

// AddErr records the Fault carried by a FaultError; other errors are ignored.
func (s *FaultSet) AddErr(err error) {
	var fe *FaultError
	if errors.As(err, &fe) {
		s.Add(fe.Fault)
	}
}

// All returns the faults in insertion order.
func (s *FaultSet) All() []Fault {
	if s == nil {
		return nil
	}
	return s.items
}

// Count returns the number of distinct faults.
func (s *FaultSet) Count() int {
	if s == nil {
		return 0
	}
	return len(s.items)
}

// Has reports whether any fault of the given kind exists.
func (s *FaultSet) Has(k Kind) bool {
	for _, f := range s.items {
		if f.Kind == k {
			return true
		}
	}
	return false
}
