// Package subtitle parses and round-trips SubRip (SRT) and WebVTT cues onto
// the shared nanosecond media time base while preserving each cue's original
// timestamp precision and text bytes.
package subtitle

import (
	"fmt"
	"math"

	"mediascan/internal/media"
)

// Format identifies the subtitle container.
type Format uint8

const (
	FormatSRT Format = iota
	FormatVTT
)

func (f Format) String() string {
	if f == FormatVTT {
		return "vtt"
	}
	return "srt"
}

// Limits bound parse work so corrupt input cannot trigger unbounded
// allocation. They are deliberately small: mediascan only handles local,
// generated fixtures.
type Limits struct {
	MaxBytes int64
	MaxCues  int
}

// DefaultLimits are used by the CLI unless overridden.
var DefaultLimits = Limits{MaxBytes: 8 << 20, MaxCues: 20000}

// Cue is one normalized subtitle record.
type Cue struct {
	// Index is the ordinal used for reporting: the source sequence number
	// when present and valid, else the 1-based position.
	Index int
	// StartNs/EndNs are exact nanosecond event times.
	StartNs int64
	EndNs   int64
	// StartRaw/EndRaw are the timestamp tokens exactly as written, so a
	// round trip can restore original precision.
	StartRaw string
	EndRaw   string
	// Text is the payload bytes (after the timing line) with line endings
	// normalized to '\n' but no other alteration.
	Text string
	// HasSettings records a VTT cue-settings suffix that was preserved.
	Settings string
	// Identifier is a VTT cue identifier line, empty for SRT.
	Identifier string
	// LineOffset is the byte offset of the cue header in the source.
	LineOffset int
}

// Document is a parsed subtitle file.
type Document struct {
	Format     Format
	Cues       []Cue
	Header     string // VTT "WEBVTT" header block including blank terminator.
	Issues     media.IssueSet
	SourceSize int64
	// TrailingNewline records whether the source ended with a newline, so
	// serialization can match it.
	TrailingNewline bool
	// RoundtripEqual is set by the caller after comparing a serialize/reparse
	// cycle; it is not produced by parsing.
	RoundtripEqual bool
}

// TimelineEvent is the normalized event the timeline engine consumes.
type TimelineEvent struct {
	TrackID uint32
	Cue     Cue
	Source  string
}

// Parse dispatches on format, applies limits, and computes RoundtripEqual by
// serializing and reparsing the document. Round-trip equality is part of the
// parse contract: any legal input must survive serialize -> reparse with
// identical normalized records.
func Parse(format Format, data []byte, limits Limits) (*Document, error) {
	var doc *Document
	var err error
	if format == FormatVTT {
		doc, err = ParseVTT(data, limits)
	} else {
		doc, err = ParseSRT(data, limits)
	}
	if err != nil || doc == nil {
		return doc, err
	}
	rendered := Serialize(doc)
	repo, rerr := parseRaw(format, []byte(rendered), limits)
	if rerr != nil {
		doc.RoundtripEqual = false
		return doc, nil
	}
	doc.RoundtripEqual = EqualRecords(doc, repo)
	return doc, nil
}

// parseRaw parses without the round-trip self-check to avoid recursion.
func parseRaw(format Format, data []byte, limits Limits) (*Document, error) {
	if format == FormatVTT {
		return ParseVTT(data, limits)
	}
	return ParseSRT(data, limits)
}

// EventTime returns the cue start on the shared base.
func (e TimelineEvent) EventTime() int64 { return e.Cue.StartNs }

// DurationNs returns the cue span.
func (e TimelineEvent) DurationNs() int64 { return e.Cue.EndNs - e.Cue.StartNs }

// parseLimits validates and applies explicit limits.
func (limits Limits) normalize() Limits {
	if limits.MaxBytes <= 0 {
		limits.MaxBytes = DefaultLimits.MaxBytes
	}
	if limits.MaxCues <= 0 {
		limits.MaxCues = DefaultLimits.MaxCues
	}
	return limits
}

// line is one physical source line with its byte offset.
type line struct {
	text   string
	offset int
}

// splitLinesNormalized splits data into lines, normalizing CRLF and lone CR to
// '\n' boundaries while retaining each line's byte offset in the source. It
// reports whether the source ended with a line terminator.
func splitLinesNormalized(data []byte) ([]line, bool) {
	lines := make([]line, 0, 64)
	start := 0
	i := 0
	for i < len(data) {
		if data[i] == '\n' || data[i] == '\r' {
			lines = append(lines, line{text: string(data[start:i]), offset: start})
			if data[i] == '\r' && i+1 < len(data) && data[i+1] == '\n' {
				i++
			}
			i++
			start = i
			continue
		}
		i++
	}
	trailing := false
	if start < len(data) {
		lines = append(lines, line{text: string(data[start:]), offset: start})
	} else {
		trailing = len(data) > 0
	}
	return lines, trailing
}

// ParseTimestamp parses a subtitle timestamp "HH:MM:SS,mmm" (SRT) or
// "HH:MM:SS.mmm" / "MM:SS.mmm" (VTT) into nanoseconds. Fractional digits
// beyond nanoseconds are preserved in raw but truncated for the numeric value,
// and a non-integral frame time is reported by the caller separately.
func ParseTimestamp(s string) (ns int64, ok bool) {
	if s == "" {
		return 0, false
	}
	// Split fractional part.
	dot := -1
	for i := 0; i < len(s); i++ {
		if s[i] == ',' || s[i] == '.' {
			dot = i
			break
		}
	}
	main := s
	frac := ""
	if dot >= 0 {
		main = s[:dot]
		frac = s[dot+1:]
	}
	parts := splitColon(main)
	if len(parts) < 2 || len(parts) > 3 {
		return 0, false
	}
	var h, m, sec int64
	var err error
	if len(parts) == 3 {
		if h, err = parseInt(parts[0]); err != nil {
			return 0, false
		}
	}
	if m, err = parseInt(parts[len(parts)-2]); err != nil {
		return 0, false
	}
	if sec, err = parseInt(parts[len(parts)-1]); err != nil {
		return 0, false
	}
	if m > 59 || sec > 59 || h < 0 || h > 9999 {
		return 0, false
	}
	// Fractional part: pad/truncate to 9 digits for ns.
	var fracNs int64
	if frac != "" {
		if len(frac) > 9 {
			frac = frac[:9]
		}
		v, err := parseInt(frac)
		if err != nil {
			return 0, false
		}
		for i := len(frac); i < 9; i++ {
			v *= 10
		}
		fracNs = v
	}
	total := h*3600 + m*60 + sec
	if total > math.MaxInt64/media.NanosPerSecond-1 {
		return 0, false
	}
	return total*media.NanosPerSecond + fracNs, true
}

func splitColon(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == ':' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

func parseInt(s string) (int64, error) {
	if s == "" || len(s) > 12 {
		return 0, fmt.Errorf("invalid integer %q", s)
	}
	var v int64
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("invalid integer %q", s)
		}
		v = v*10 + int64(c-'0')
		if v < 0 {
			return 0, fmt.Errorf("integer overflow in %q", s)
		}
	}
	return v, nil
}

// FormatTimestamp renders ns as SRT (comma) or VTT (dot) with millisecond
// precision, the canonical form used when synthesizing output.
func FormatTimestamp(ns int64, format Format) string {
	sep := ','
	if format == FormatVTT {
		sep = '.'
	}
	if ns < 0 {
		ns = 0
	}
	ms := ns / 1_000_000
	h := ms / 3_600_000
	ms -= h * 3_600_000
	m := ms / 60_000
	ms -= m * 60_000
	sec := ms / 1000
	ms -= sec * 1000
	return fmt.Sprintf("%02d:%02d:%02d%c%03d", h, m, sec, sep, ms)
}
