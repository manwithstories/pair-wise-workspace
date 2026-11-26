package model

import "fmt"

// ErrorKind enumerates the structured failure conditions the pipeline can
// report. Degraded conditions are reported through an ErrorLog rather than
// aborting the run: a damaged archive still produces a complete summary.
type ErrorKind string

const (
	// ErrBlockMissing: a whole data block is absent from the carrier.
	ErrBlockMissing ErrorKind = "block_missing"
	// ErrBlockCRC: a present block failed its CRC32 check.
	ErrBlockCRC ErrorKind = "block_crc"
	// ErrTruncated: the carrier ends before its declared PCM payload.
	ErrTruncated ErrorKind = "truncated"
	// ErrUnrecovered: missing/corrupt blocks exceeded parity capacity.
	ErrUnrecovered ErrorKind = "unrecovered"
	// ErrLayoutMissing: no channel layout is associated with a track.
	ErrLayoutMissing ErrorKind = "layout_missing"
	// ErrLayoutConflict: two fragments disagree about channel ordering.
	ErrLayoutConflict ErrorKind = "layout_conflict"
	// ErrColorConflict: color/label metadata disagrees across fragments.
	ErrColorConflict ErrorKind = "color_conflict"
	// ErrNoFrames: no complete sample frame can be recovered at all.
	ErrNoFrames ErrorKind = "no_frames"
	// ErrInvalidParam: a CLI or caller parameter is invalid.
	ErrInvalidParam ErrorKind = "invalid_param"
	// ErrPTSNotMonotonic: a PTS ordering invariant was violated.
	ErrPTSNotMonotonic ErrorKind = "pts_not_monotonic"
	// ErrUnsupportedFormat: PCM format outside the supported envelope.
	ErrUnsupportedFormat ErrorKind = "unsupported_format"
)

// NoPosition marks an inapplicable block or sample position.
const NoPosition int64 = -1

// TrackedError is a failure annotated with the track, block id and sample
// position it relates to, so every reported defect is traceable.
type TrackedError struct {
	Kind   ErrorKind
	Track  string
	Block  int
	Sample int64
	Msg    string
}

// Error implements the error interface.
func (e TrackedError) Error() string {
	loc := e.Track
	if e.Block >= 0 {
		loc = fmt.Sprintf("%s/block#%d", loc, e.Block)
	}
	if e.Sample != NoPosition {
		loc = fmt.Sprintf("%s@sample%d", loc, e.Sample)
	}
	if e.Msg == "" {
		return fmt.Sprintf("%s: %s", e.Kind, loc)
	}
	return fmt.Sprintf("%s: %s: %s", e.Kind, loc, e.Msg)
}

// ErrorLog collects structured defects in encounter order. It is safe for
// append-only use within a single pipeline run.
type ErrorLog struct {
	items []TrackedError
}

// NewErrorLog creates an empty error log.
func NewErrorLog() *ErrorLog { return &ErrorLog{} }

// Add records a structured defect.
func (l *ErrorLog) Add(e TrackedError) {
	if e.Sample == 0 {
		e.Sample = NoPosition
	}
	l.items = append(l.items, e)
}

// Addf is a convenience constructor mirroring fmt.Sprintf.
func (l *ErrorLog) Addf(kind ErrorKind, track string, block int, sample int64, format string, args ...any) {
	l.Add(TrackedError{
		Kind:   kind,
		Track:  track,
		Block:  block,
		Sample: sample,
		Msg:    fmt.Sprintf(format, args...),
	})
}

// All returns the recorded defects in encounter order.
func (l *ErrorLog) All() []TrackedError {
	if l == nil {
		return nil
	}
	return append([]TrackedError(nil), l.items...)
}

// Count returns how many defects of the given kind were recorded.
func (l *ErrorLog) Count(kind ErrorKind) int {
	n := 0
	for _, e := range l.items {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

// Has reports whether any defect of the given kind was recorded.
func (l *ErrorLog) Has(kind ErrorKind) bool { return l.Count(kind) > 0 }

// Degraded reports whether any defect at all was recorded.
func (l *ErrorLog) Degraded() bool { return len(l.items) > 0 }
