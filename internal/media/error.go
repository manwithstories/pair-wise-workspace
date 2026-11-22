// Package media defines the shared time base and the unified, locatable,
// severity-graded and recoverability-aware error model used by every
// subsystem of mediascan.
package media

import (
	"fmt"
	"sort"
	"strings"
)

// Severity grades how much of the pipeline an issue affects.
type Severity uint8

const (
	// SeverityInfo describes a benign observation (sequence wrap, reordering)
	// that is recorded for auditability but does not degrade output.
	SeverityInfo Severity = iota
	// SeverityRecoverable marks a fault that is isolated to a locatable range;
	// the rest of the track continues to be produced.
	SeverityRecoverable
	// SeverityFatal marks input that prevents any trustworthy processing.
	SeverityFatal
)

func (s Severity) String() string {
	switch s {
	case SeverityInfo:
		return "info"
	case SeverityRecoverable:
		return "recoverable"
	case SeverityFatal:
		return "fatal"
	default:
		return "unknown"
	}
}

// Class is a stable machine-readable identifier for an issue category. The
// strings are part of the CLI contract and must not change casually.
type Class string

const (
	// Subtitle classes.
	ClassSubtitleInvalidIndex   Class = "subtitle.invalid-index"
	ClassSubtitleNegativeRange  Class = "subtitle.negative-duration"
	ClassSubtitleTruncatedCue   Class = "subtitle.truncated-cue"
	ClassSubtitleBadTimestamp   Class = "subtitle.malformed-timestamp"
	ClassSubtitleTooManyCues    Class = "subtitle.too-many-cues"
	ClassSubtitleOversizedInput Class = "subtitle.oversized-input"

	// Clock / timecode classes.
	ClassTimecodeBadFormat       Class = "timecode.malformed"
	ClassTimecodeOutOfRange      Class = "timecode.out-of-range"
	ClassTimecodeBadDropFrame    Class = "timecode.invalid-drop-frame"
	ClassTimecodeNonIntegralTime Class = "timecode.non-integral-frame"

	// MP4 / stream classes.
	ClassMP4BadStructure    Class = "mp4.bad-structure"
	ClassMP4Truncated       Class = "mp4.truncated"
	ClassMP4CorruptSample   Class = "mp4.corrupt-sample-size"
	ClassMP4MissingIndex    Class = "mp4.missing-index"
	ClassMP4NoKeyframes     Class = "mp4.no-keyframes"
	ClassMP4IndexIncomplete Class = "mp4.index-incomplete"

	// RTP classes.
	ClassRTPTruncated     Class = "rtp.truncated-packet"
	ClassRTPSequenceWrap  Class = "rtp.sequence-wrap"
	ClassRTPDuplicate     Class = "rtp.duplicate-packet"
	ClassRTPOutOfOrder    Class = "rtp.out-of-order-packet"
	ClassRTPInvalidHeader Class = "rtp.invalid-header"

	// Timeline classes.
	ClassTimelineLateEvent     Class = "timeline.late-event"
	ClassTimelineReplay        Class = "timeline.replay"
	ClassTimelineWatermarkTime Class = "timeline.bad-watermark"

	// Segment / pipeline classes.
	ClassSegmentHashMismatch  Class = "segment.hash-mismatch"
	ClassPipelineInconsistent Class = "pipeline.inconsistent"
)

// Locator pins an issue to a concrete source position. Any subset of fields
// may be set; consumers must not assume all of them are meaningful.
type Locator struct {
	TrackID   uint32
	TrackKind string
	// ByteOffset/ByteLength locate a byte range inside the input file.
	ByteOffset int64
	ByteLength int64
	// PacketIndex is the zero-based index in the packet sequence; PacketSeq
	// is the RTP sequence number carried in the header.
	PacketIndex int
	PacketSeq   uint16
	// CueIndex is the subtitle cue ordinal (1-based) or 0 when unknown.
	CueIndex int
	// TimeNs is the event time on the shared nanosecond base, when known.
	TimeNs int64
}

func (l Locator) String() string {
	var b strings.Builder
	if l.TrackID != 0 || l.TrackKind != "" {
		fmt.Fprintf(&b, "track=%d", l.TrackID)
		if l.TrackKind != "" {
			fmt.Fprintf(&b, "(%s)", l.TrackKind)
		}
	}
	if l.PacketIndex != 0 || l.PacketSeq != 0 {
		fmt.Fprintf(&b, " packet=%d seq=%d", l.PacketIndex, l.PacketSeq)
	}
	if l.CueIndex != 0 {
		fmt.Fprintf(&b, " cue=%d", l.CueIndex)
	}
	if l.ByteLength > 0 || l.ByteOffset > 0 {
		fmt.Fprintf(&b, " bytes=[%d,%d)", l.ByteOffset, l.ByteOffset+l.ByteLength)
	}
	if l.TimeNs != 0 {
		fmt.Fprintf(&b, " t_ns=%d", l.TimeNs)
	}
	if b.Len() == 0 {
		return "loc=<none>"
	}
	return "loc=" + b.String()
}

// Issue is a single fault or observation produced by a subsystem.
type Issue struct {
	Class       Class
	Severity    Severity
	Recoverable bool
	Message     string
	Locator     Locator
}

func (i Issue) Error() string {
	return fmt.Sprintf("%s [%s]: %s (%s)", i.Class, i.Severity, i.Message, i.Locator)
}

// NewIssue constructs a recoverable issue, the common case.
func NewIssue(class Class, loc Locator, format string, args ...any) Issue {
	return Issue{
		Class:       class,
		Severity:    SeverityRecoverable,
		Recoverable: true,
		Message:     fmt.Sprintf(format, args...),
		Locator:     loc,
	}
}

// NewFatal constructs a non-recoverable issue.
func NewFatal(class Class, loc Locator, format string, args ...any) Issue {
	return Issue{
		Class:       class,
		Severity:    SeverityFatal,
		Recoverable: false,
		Message:     fmt.Sprintf(format, args...),
		Locator:     loc,
	}
}

// NewInfo constructs a benign observation.
func NewInfo(class Class, loc Locator, format string, args ...any) Issue {
	return Issue{
		Class:       class,
		Severity:    SeverityInfo,
		Recoverable: true,
		Message:     fmt.Sprintf(format, args...),
		Locator:     loc,
	}
}

// IssueSet is a deterministic collection of issues. Callers append in any
// order and call Sort before rendering or hashing.
type IssueSet []Issue

func (s *IssueSet) Add(issues ...Issue) {
	*s = append(*s, issues...)
}

// Fatal reports whether any issue is non-recoverable.
func (s IssueSet) Fatal() bool {
	for _, i := range s {
		if i.Severity == SeverityFatal {
			return true
		}
	}
	return false
}

// RecoverableCount counts issues that degrade output but do not abort it.
func (s IssueSet) RecoverableCount() int {
	n := 0
	for _, i := range s {
		if i.Severity == SeverityRecoverable {
			n++
		}
	}
	return n
}

// CountByClass returns a deterministic class -> count map.
func (s IssueSet) CountByClass() map[Class]int {
	m := make(map[Class]int, len(s))
	for _, i := range s {
		m[i.Class]++
	}
	return m
}

// Classes returns the distinct classes present, sorted.
func (s IssueSet) Classes() []Class {
	seen := make(map[Class]struct{}, len(s))
	out := make([]Class, 0, len(s))
	for _, i := range s {
		if _, ok := seen[i.Class]; ok {
			continue
		}
		seen[i.Class] = struct{}{}
		out = append(out, i.Class)
	}
	sort.Slice(out, func(a, b int) bool { return out[a] < out[b] })
	return out
}

// Sort imposes a total order so that rendering and hashing are stable across
// runs regardless of discovery order.
func (s IssueSet) Sort() {
	sort.SliceStable(s, func(a, b int) bool {
		x, y := s[a], s[b]
		if x.Locator.TrackID != y.Locator.TrackID {
			return x.Locator.TrackID < y.Locator.TrackID
		}
		if x.Class != y.Class {
			return x.Class < y.Class
		}
		if x.Locator.ByteOffset != y.Locator.ByteOffset {
			return x.Locator.ByteOffset < y.Locator.ByteOffset
		}
		if x.Locator.PacketIndex != y.Locator.PacketIndex {
			return x.Locator.PacketIndex < y.Locator.PacketIndex
		}
		if x.Locator.CueIndex != y.Locator.CueIndex {
			return x.Locator.CueIndex < y.Locator.CueIndex
		}
		if x.Locator.TimeNs != y.Locator.TimeNs {
			return x.Locator.TimeNs < y.Locator.TimeNs
		}
		return x.Message < y.Message
	})
}
