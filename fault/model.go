// Package fault defines the shared error-range model used by ingest, recovery,
// subtitles, GOP indexing and boundary checks.
package fault

import (
	"sort"
	"sync"
)

// Kind identifies a class of transport, coding or editorial fault.
type Kind string

const (
	DuplicatePacket  Kind = "duplicate_packet"
	ReorderedPacket  Kind = "reordered_packet"
	Gap              Kind = "sequence_gap"
	BurstLoss        Kind = "burst_loss"
	BadCRC           Kind = "bad_crc"
	TruncatedHeader  Kind = "truncated_header"
	InvalidTimecode  Kind = "invalid_timecode"
	MalformedCue     Kind = "malformed_cue"
	OverlappingCue   Kind = "overlapping_cue"
	TruncatedCue     Kind = "truncated_cue"
	BandwidthDrop    Kind = "bandwidth_drop"
	UnrecoveredBlock Kind = "unrecovered_block"
	BadSegment       Kind = "bad_segment"
	BoundaryConflict Kind = "boundary_conflict"
)

// Span is an inclusive/exclusive range in packet-sequence and millisecond time.
// Sequence ranges are inclusive; millisecond ranges are [StartMS, EndMS).
type Span struct {
	StartSeq uint64 `json:"start_seq,omitempty"`
	EndSeq   uint64 `json:"end_seq,omitempty"`
	StartMS  int64  `json:"start_ms,omitempty"`
	EndMS    int64  `json:"end_ms,omitempty"`
}

// Fault is one source-localized error interval.
type Fault struct {
	Kind       Kind   `json:"kind"`
	Camera     string `json:"camera,omitempty"`
	Stream     string `json:"stream,omitempty"`
	Segment    int    `json:"segment,omitempty"`
	Span       Span   `json:"span"`
	SourceLine int    `json:"source_line,omitempty"`
	Message    string `json:"message,omitempty"`
	Recovered  bool   `json:"recovered"`
}

// Model is a concurrency-safe fault collector.
type Model struct {
	mu     sync.RWMutex
	faults []Fault
}

// NewModel creates an empty fault model.
func NewModel() *Model { return &Model{} }

// Add records a fault and returns a copy of the stored value.
func (m *Model) Add(f Fault) Fault {
	if m == nil {
		return f
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.faults = append(m.faults, f)
	return f
}

// AddKind is a concise helper for recording a typed fault.
func (m *Model) AddKind(kind Kind, camera, stream string, span Span, msg string) Fault {
	return m.Add(Fault{Kind: kind, Camera: camera, Stream: stream, Span: span, Message: msg})
}

// All returns a sorted copy. Sorting keeps reports and hashes deterministic.
func (m *Model) All() []Fault {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	out := append([]Fault(nil), m.faults...)
	m.mu.RUnlock()
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Segment != b.Segment {
			return a.Segment < b.Segment
		}
		if a.SourceLine != b.SourceLine {
			return a.SourceLine < b.SourceLine
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Span.StartSeq < b.Span.StartSeq
	})
	return out
}

// OfKinds returns faults whose kind is in kinds.
func (m *Model) OfKinds(kinds ...Kind) []Fault {
	all := m.All()
	want := make(map[Kind]bool, len(kinds))
	for _, k := range kinds {
		want[k] = true
	}
	out := make([]Fault, 0, len(all))
	for _, f := range all {
		if want[f.Kind] {
			out = append(out, f)
		}
	}
	return out
}

// Count returns the number of recorded faults of one kind.
func (m *Model) Count(kind Kind) int { return len(m.OfKinds(kind)) }

// MarkRecovered marks all faults overlapping pred as recovered.
func (m *Model) MarkRecovered(kind Kind, segment int, startSeq, endSeq uint64) int {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for i := range m.faults {
		f := &m.faults[i]
		if f.Kind != kind || f.Segment != segment || f.Recovered {
			continue
		}
		if f.Span.EndSeq >= startSeq && f.Span.StartSeq <= endSeq {
			f.Recovered = true
			n++
		}
	}
	return n
}

// UnrecoveredBadSegments returns precise segment-level faults that remain unsafe.
func (m *Model) UnrecoveredBadSegments() []Fault {
	return m.OfKinds(BadSegment)
}

// MergeSequenceSpans coalesces sorted inclusive sequence spans.
func MergeSequenceSpans(spans []Span) []Span {
	if len(spans) == 0 {
		return nil
	}
	sorted := append([]Span(nil), spans...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].StartSeq < sorted[j].StartSeq })
	out := []Span{sorted[0]}
	for _, s := range sorted[1:] {
		last := &out[len(out)-1]
		if s.StartSeq <= last.EndSeq+1 {
			if s.EndSeq > last.EndSeq {
				last.EndSeq = s.EndSeq
			}
			continue
		}
		out = append(out, s)
	}
	return out
}
