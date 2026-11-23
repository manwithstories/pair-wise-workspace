package timeline

import "fmt"

// ErrCode classifies pipeline errors so callers can map them to exit codes.
type ErrCode string

const (
	ErrDuplicateSeq   ErrCode = "duplicate_sequence"
	ErrInvalidDur     ErrCode = "invalid_duration"
	ErrMissingRef     ErrCode = "missing_reference"
	ErrTimestampRange ErrCode = "timestamp_out_of_range"
	ErrGap            ErrCode = "unrecoverable_gap"
	ErrTruncated      ErrCode = "truncated_segment"
	ErrColorConflict  ErrCode = "color_metadata_conflict"
	ErrHashConflict   ErrCode = "fingerprint_conflict"
	ErrBoundary       ErrCode = "unrepresentable_boundary"
	ErrLimit          ErrCode = "limit_exceeded"
	ErrDigest         ErrCode = "digest_mismatch"
)

// Error locates a failure to a segment, an optional packet sequence number
// and a time interval (milliseconds on the unified clock).
type Error struct {
	Code    ErrCode
	Segment string
	HasSeq  bool
	Seq     uint64
	StartMs int64
	EndMs   int64
	Msg     string
}

func (e *Error) Error() string {
	loc := e.Segment
	if loc == "" {
		loc = "<stream>"
	}
	s := fmt.Sprintf("%s: %s", e.Code, loc)
	if e.HasSeq {
		s += fmt.Sprintf(" seq=%d", e.Seq)
	}
	s += fmt.Sprintf(" [%dms,%dms]", e.StartMs, e.EndMs)
	if e.Msg != "" {
		s += ": " + e.Msg
	}
	return s
}

// New builds an Error with a located interval.
func New(code ErrCode, segment string, startMs, endMs int64, msg string) *Error {
	return &Error{Code: code, Segment: segment, StartMs: startMs, EndMs: endMs, Msg: msg}
}

// NewSeq builds an Error that also carries a packet sequence number.
func NewSeq(code ErrCode, segment string, seq uint64, startMs, endMs int64, msg string) *Error {
	e := New(code, segment, startMs, endMs, msg)
	e.HasSeq, e.Seq = true, seq
	return e
}
