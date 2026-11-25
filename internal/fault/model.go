// Package fault defines the shared error model. Every module reports
// damage as a Span on the common rational clock so parse, repair and
// pipeline diagnostics merge into one consistent picture.
package fault

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"

	"fieldqc/internal/clock"
)

// Code classifies a fault span.
type Code string

const (
	CodeTruncatedBox     Code = "TRUNCATED_BOX"
	CodeEditRange        Code = "EDIT_RANGE"
	CodeNegativeDuration Code = "NEGATIVE_DURATION"
	CodeOverflow         Code = "OVERFLOW"
	CodeInvalidEdit      Code = "INVALID_EDIT"
	CodeTimestampJump    Code = "TIMESTAMP_JUMP"
	CodeRTPUnrecoverable Code = "RTP_UNRECOVERABLE"
	CodeColorConflict    Code = "COLOR_CONFLICT"
	CodeAudioEdge        Code = "AUDIO_EDGE"
)

// Span locates a fault on a track's media timeline [Start, End).
type Span struct {
	Track  string      `json:"track"`
	Code   Code        `json:"code"`
	Start  clock.Ratio `json:"start"`
	End    clock.Ratio `json:"end"`
	Detail string      `json:"detail,omitempty"`
}

func (s Span) String() string {
	return fmt.Sprintf("%s|%s|%s|%s|%s", s.Track, s.Code, s.Start, s.End, s.Detail)
}

// Model collects spans reported by the analyzers.
type Model struct {
	spans []Span
}

func NewModel() *Model { return &Model{} }

// Add records a span, sanitizing zero-value times and inverted ranges.
func (m *Model) Add(s Span) {
	if s.Start.Den == 0 {
		s.Start = clock.Zero()
	}
	if s.End.Den == 0 || s.End.Cmp(s.Start) < 0 {
		s.End = s.Start
	}
	m.spans = append(m.spans, s)
}

// Len returns the number of recorded spans.
func (m *Model) Len() int { return len(m.spans) }

// Spans returns all spans sorted by track, start time and code.
func (m *Model) Spans() []Span {
	out := make([]Span, len(m.spans))
	copy(out, m.spans)
	SortSpans(out)
	return out
}

// SortSpans sorts spans deterministically in place.
func SortSpans(s []Span) {
	sort.SliceStable(s, func(i, j int) bool {
		if s[i].Track != s[j].Track {
			return s[i].Track < s[j].Track
		}
		if c := s[i].Start.Cmp(s[j].Start); c != 0 {
			return c < 0
		}
		return s[i].Code < s[j].Code
	})
}

// Digest is a stable hash of the sorted spans: identical inputs produce
// identical digests regardless of insertion order.
func (m *Model) Digest() string {
	h := sha256.New()
	for _, s := range m.Spans() {
		h.Write([]byte(s.String() + "\n"))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}
