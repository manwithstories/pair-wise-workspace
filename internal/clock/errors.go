// Package clock defines the shared nanosecond time base, the unified
// structured error type used across the pipeline and RTP-style boundary
// models (timestamp wrap / sequence rollover) reused from pion/rtp.
package clock

import (
	"errors"
	"fmt"

	"github.com/pion/rtp"
)

// Category classifies every pipeline failure into a stable string so that
// callers can distinguish structural corruption from runtime/checkpoint
// failures without parsing human-readable messages.
type Category string

const (
	// CatTruncated means the reader hit EOF inside a declared element.
	CatTruncated Category = "truncated_input"
	// CatOversized means an element declared a length above the budget.
	CatOversized Category = "oversized_element"
	// CatOverflow means an integer/offset computation would overflow.
	CatOverflow Category = "integer_overflow"
	// CatBadVINT means a variable-length integer could not be decoded.
	CatBadVINT Category = "invalid_vint"
	// CatBadPath means navigation left the declared element boundaries.
	CatBadPath Category = "path_out_of_bounds"
	// CatBadBlock means a block header/body could not be decoded.
	CatBadBlock Category = "invalid_block"
	// CatLateEvent means an event arrived after the committed watermark.
	CatLateEvent Category = "late_event"
	// CatAlignment means representation boundaries could not be aligned.
	CatAlignment Category = "alignment_failure"
	// CatCheckpoint means resume state or an output digest did not match.
	CatCheckpoint Category = "checkpoint_mismatch"
	// CatCancelled means the context was cancelled (slow consumer / user).
	CatCancelled Category = "cancelled"
)

// MediaError is the single error type produced by every pipeline layer.
// Offset is the byte position in the input when known, Track is the EBML
// track number when known (0 means "not track specific").
type MediaError struct {
	Category Category
	Offset   int64
	Track    uint64
	Message  string
	Cause    error
}

func (e *MediaError) Error() string {
	s := string(e.Category)
	if e.Offset >= 0 {
		s += fmt.Sprintf(" at byte %d", e.Offset)
	}
	if e.Track > 0 {
		s += fmt.Sprintf(" track %d", e.Track)
	}
	if e.Message != "" {
		s += ": " + e.Message
	}
	if e.Cause != nil {
		s += ": " + e.Cause.Error()
	}
	return s
}

// Unwrap enables errors.Is/As on the wrapped cause.
func (e *MediaError) Unwrap() error { return e.Cause }

// NewError builds a MediaError with an unknown offset.
func NewError(c Category, format string, args ...any) *MediaError {
	return &MediaError{Category: c, Offset: -1, Message: fmt.Sprintf(format, args...)}
}

// AtError builds a MediaError anchored at a concrete input byte offset.
func AtError(c Category, offset int64, format string, args ...any) *MediaError {
	return &MediaError{Category: c, Offset: offset, Message: fmt.Sprintf(format, args...)}
}

// AsMediaError extracts a *MediaError from any wrapped error chain.
func AsMediaError(err error) (*MediaError, bool) {
	var me *MediaError
	if errors.As(err, &me) {
		return me, true
	}
	return nil, false
}

// RTP boundary constants reused for the wrap model. RTP sequence numbers
// are uint16 and timestamps uint32; Matroska block timecodes are uint16,
// so the same modular arithmetic applies to both.
const (
	SeqMod      = int64(1) << 16 // rtp.SequenceNumber width
	TimeMod     = int64(1) << 32 // rtp.Timestamp width
	HalfSeqMod  = SeqMod / 2
	HalfTimeMod = TimeMod / 2
)

// RTPHeader is the boundary model borrowed from pion/rtp v1.8.11: the
// uint16 SequenceNumber and uint32 Timestamp fields define the modular
// arithmetic domains our unwrap helpers operate on.
type RTPHeader = rtp.Header
