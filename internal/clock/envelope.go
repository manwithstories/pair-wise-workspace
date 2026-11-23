package clock

import (
	"errors"
	"fmt"
	"strings"
)

// Category classifies every failure the pipeline can report. Categories are
// stable strings so that the terminal report and tests can assert on them.
type Category string

const (
	CatArgument    Category = "argument"     // bad CLI flag or API parameter
	CatTimebase    Category = "timebase"     // rational conversion failure
	CatTimeline    Category = "timeline"     // invalid clip, overlap or transition
	CatGraph       Category = "graph"        // dependency cycle or missing node
	CatContainer   Category = "container"    // MP4 box structure is unusable
	CatSampleTable Category = "sample_table" // stsz/stco/stsc inconsistent
	CatSample      Category = "sample"       // one sample is truncated or corrupt
	CatIndexGap    Category = "index_gap"    // sample table missing while mdat has data
	CatTransport   Category = "transport"    // RTP loss/reorder/jitter budget
	CatSync        Category = "sync"         // audio/video drift beyond limit
	CatRender      Category = "render"       // frame composition or PNG write
	CatInternal    Category = "internal"     // invariant violated in our code
)

// Recovery records what the system did about a failure, so a reviewer can
// replay the decision without reading code.
type Recovery string

const (
	RecoveredNone        Recovery = "none"             // unrecoverable, abort the unit
	RecoveredSkipped     Recovery = "skip"             // drop the affected sample/frame
	RecoveredPrefix      Recovery = "keep_prefix"      // keep valid prefix, mark gap
	RecoveredResync      Recovery = "resync"           // scan forward to sync sample
	RecoveredSilence     Recovery = "pad_silence"      // fill audio with samples of silence
	RecoveredDropPacket  Recovery = "drop_packet"      // discard an expired RTP packet
	RecoveredKeyframeReq Recovery = "request_keyframe" // wait for next keyframe
	RecoveredPartial     Recovery = "partial_report"   // emit partial result, exit 2
)

// Span locates a failure inside the media on the timeline tick axis. Track
// and Segment are empty when a layer cannot attribute the failure; Frame is
// -1 when the failure does not concern a rendered frame.
type Span struct {
	Track    string
	Segment  string
	Interval Interval
	Frame    int
}

// NewSpan builds a span with Frame=-1 so the zero value stays printable.
func NewSpan(track, segment string, iv Interval) Span {
	return Span{Track: track, Segment: segment, Interval: iv, Frame: -1}
}

func (s Span) String() string {
	var b strings.Builder
	if s.Track != "" {
		b.WriteString(s.Track)
		b.WriteString("/")
	}
	if s.Segment != "" {
		b.WriteString(s.Segment)
		b.WriteString("@")
	}
	b.WriteString(s.Interval.String())
	return b.String()
}

// Error is the single propagatable error type. It wraps an underlying cause
// and adds category, span and the recovery that was already attempted.
type Error struct {
	Category Category
	Span     Span
	Recovery Recovery
	Message  string
	Err      error
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString(string(e.Category))
	if e.Span != (Span{}) {
		b.WriteString(" ")
		b.WriteString(e.Span.String())
	}
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	if e.Err != nil {
		b.WriteString(": ")
		b.WriteString(e.Err.Error())
	}
	if e.Recovery != RecoveredNone {
		b.WriteString(" [recovery=")
		b.WriteString(string(e.Recovery))
		b.WriteString("]")
	}
	return b.String()
}

func (e *Error) Unwrap() error { return e.Err }

// NewError builds a classified error.
func NewError(cat Category, span Span, rec Recovery, msg string, cause error) *Error {
	return &Error{Category: cat, Span: span, Recovery: rec, Message: msg, Err: cause}
}

// Errorf is the format-string form of NewError.
func Errorf(cat Category, span Span, rec Recovery, format string, args ...any) *Error {
	return NewError(cat, span, rec, fmt.Sprintf(format, args...), nil)
}

// WithSpan returns a copy of e carrying a different span; used when a lower
// layer does not know the segment name but the caller does.
func WithSpan(err error, span Span) error {
	var ce *Error
	if errors.As(err, &ce) {
		cp := *ce
		if span.Track != "" {
			cp.Span.Track = span.Track
		}
		if span.Segment != "" {
			cp.Span.Segment = span.Segment
		}
		if span.Interval != (Interval{}) {
			cp.Span.Interval = span.Interval
		}
		if span.Frame >= 0 {
			cp.Span.Frame = span.Frame
		}
		return &cp
	}
	if err == nil {
		return nil
	}
	return &Error{Category: CatInternal, Span: span, Message: err.Error(), Err: err}
}

// WithRecovery returns a copy of e carrying the recovery actually performed.
func WithRecovery(err error, rec Recovery) error {
	var ce *Error
	if errors.As(err, &ce) {
		cp := *ce
		cp.Recovery = rec
		return &cp
	}
	if err == nil {
		return nil
	}
	return &Error{Category: CatInternal, Recovery: rec, Message: err.Error(), Err: err}
}

// Classify maps any error onto a category; unknown errors become internal.
// It never returns nil for a non-nil err.
func Classify(err error) Category {
	var ce *Error
	if errors.As(err, &ce) {
		return ce.Category
	}
	if err == nil {
		return ""
	}
	return CatInternal
}

// IsRecoverable reports whether the error was already handled in a way that
// still yields reviewable output.
func IsRecoverable(err error) bool {
	var ce *Error
	if errors.As(err, &ce) {
		return ce.Recovery != RecoveredNone
	}
	return false
}
