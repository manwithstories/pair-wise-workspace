// Package fault defines the unified error model that the TS/RTP parsers hand
// to the scanner. Every diagnostic carries a byte offset into the input so the
// report can locate where the stream first diverged and whether a gap is
// genuinely recoverable or the start of a chain of downstream corruption.
package fault

import (
	"fmt"
	"sort"
	"strings"
)

// Kind classifies how a stream deviated from the expected structure.
type Kind string

const (
	// KindPSI covers PSI structural problems: a PAT/PMT that spans more packets
	// than the section declares, a table that never completes, or a version
	// switch observed mid-section.
	KindPSI Kind = "psi"
	// KindCRC is a PSI section whose CRC-32 does not match its payload.
	KindCRC Kind = "crc"
	// KindContinuity is a continuity_counter discontinuity on an established
	// PID, i.e. a lost or duplicated packet.
	KindContinuity Kind = "continuity"
	// KindTruncated is a packet cut short: fewer than 188 bytes, or a packet
	// whose adaptation field claims more bytes than remain.
	KindTruncated Kind = "truncated"
	// KindSync is a loss of the 0x47 sync byte where a packet boundary was
	// expected.
	KindSync Kind = "sync"
	// KindAdaptation is a malformed adaptation field, e.g. stuffing that
	// overruns the packet.
	KindAdaptation Kind = "adaptation"
	// KindRTP covers transport-level RTP defects surfaced before depacketising.
	KindRTP Kind = "rtp"
	// KindMetadata flags a track whose parameters differ from another track in a
	// way that forbids silent merging (channel layout, sample rate, colour).
	KindMetadata Kind = "metadata"
)

// Recoverable reports whether a gap caused by this kind can be healed by
// skipping ahead to the next random-access point. Structural corruption
// (truncated packets, lost PSI) marks the start of a damaged range, but the
// range itself ends at the next clean random-access frame.
func (k Kind) Recoverable() bool {
	switch k {
	case KindContinuity, KindTruncated, KindSync, KindAdaptation, KindCRC:
		return true
	}
	return false
}

// Severity orders diagnostics for the report. The scanner stops counting a
// contiguous damaged region once severity drops and the stream re-synchronises.
type Severity int

const (
	// SevInfo is reported but does not break a range.
	SevInfo Severity = iota
	// SevRepairable is a gap healed by keyframe recovery.
	SevRepairable
	// SevFatal marks unrecoverable corruption, ending a sample.
	SevFatal
)

// SeverityOf maps a kind onto its report severity.
func SeverityOf(k Kind) Severity {
	switch k {
	case KindMetadata, KindRTP:
		return SevInfo
	case KindContinuity, KindTruncated, KindSync, KindAdaptation:
		return SevRepairable
	case KindCRC, KindPSI:
		return SevFatal
	}
	return SevInfo
}

// Error is one located stream defect.
type Error struct {
	Kind   Kind     `json:"kind"`
	Offset int64    `json:"offset"`
	PID    uint16   `json:"pid,omitempty"`
	Packet int64    `json:"packet,omitempty"`
	Ticks  int64    `json:"ticks,omitempty"`
	Detail string   `json:"detail,omitempty"`
	Chain  []string `json:"chain,omitempty"`
}

// String renders a compact one-line diagnostic; stable ordering keeps demo
// output hashable.
func (e Error) String() string {
	var b strings.Builder
	b.WriteString(string(e.Kind))
	if e.PID != 0 {
		fmt.Fprintf(&b, " pid=0x%04x", e.PID)
	}
	fmt.Fprintf(&b, " off=%d", e.Offset)
	if e.Packet > 0 {
		fmt.Fprintf(&b, " pkt=%d", e.Packet)
	}
	if e.Detail != "" {
		b.WriteString(" ")
		b.WriteString(e.Detail)
	}
	return b.String()
}

// List is an ordered collection of diagnostics. It keeps a stable sort by
// offset then kind so repeated scans of the same input produce identical
// output.
type List struct {
	items []Error
}

// Add appends a diagnostic.
func (l *List) Add(e Error) { l.items = append(l.items, e) }

// Len reports the number of diagnostics.
func (l *List) Len() int { return len(l.items) }

// Items returns the slice for read-only iteration.
func (l *List) Items() []Error { return l.items }

// Sorted returns diagnostics ordered by offset, then kind, then PID. The
// underlying list is unchanged.
func (l *List) Sorted() []Error {
	out := append([]Error(nil), l.items...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Offset != out[j].Offset {
			return out[i].Offset < out[j].Offset
		}
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].PID < out[j].PID
	})
	return out
}

// FirstFatal returns the earliest offset at which an unrecoverable error was
// recorded, and false if none was seen. Used by the demo report to describe
// the "first error offset" invariant.
func (l *List) FirstFatal() (int64, bool) {
	first := int64(-1)
	for _, e := range l.items {
		if SeverityOf(e.Kind) != SevFatal {
			continue
		}
		if first < 0 || e.Offset < first {
			first = e.Offset
		}
	}
	return first, first >= 0
}

// First returns the earliest offset of any diagnostic, or -1.
func (l *List) First() int64 {
	first := int64(-1)
	for _, e := range l.items {
		if first < 0 || e.Offset < first {
			first = e.Offset
		}
	}
	return first
}
