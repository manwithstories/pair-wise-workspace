// Package fault defines the unified error model shared by every mediaroute
// subsystem. Errors carry a machine-readable category plus optional track,
// sample, packet and time-range locators so a failure at the packet layer
// can be correlated with sample-level damage.
package fault

import (
	"errors"
	"fmt"

	"mediaroute/internal/clock"
)

// Category classifies every error produced by the pipeline.
type Category string

const (
	// CatInvalidTimeBase: zero/negative timescale or unsupported rational.
	CatInvalidTimeBase Category = "invalid_time_base"
	// CatTimestampOverflow: a timestamp conversion would overflow int64.
	CatTimestampOverflow Category = "timestamp_overflow"
	// CatTruncatedSampleTable: a container sample table is incomplete.
	CatTruncatedSampleTable Category = "truncated_sample_table"
	// CatIllegalCrop: clap/crop rectangle falls outside the coded frame.
	CatIllegalCrop Category = "illegal_crop"
	// CatMetadataConflict: rotation/colour metadata disagrees across scopes.
	CatMetadataConflict Category = "metadata_conflict"
	// CatUnsupportedCodec: the codec/structure cannot be fully interpreted.
	CatUnsupportedCodec Category = "unsupported_codec"
	// CatPacketGap: an unrecoverable RTP sequence gap was detected.
	CatPacketGap Category = "packet_gap"
	// CatSampleDamage: one or more complete samples could not be reconstructed.
	CatSampleDamage Category = "sample_damage"
	// CatIndexInvalid: an index file failed checksum/version/parameter checks.
	CatIndexInvalid Category = "index_invalid"
	// CatCheckpointMismatch: a checkpoint failed its three-way verification.
	CatCheckpointMismatch Category = "checkpoint_mismatch"
	// CatLimitExceeded: a file size or packet cache limit was exceeded.
	CatLimitExceeded Category = "limit_exceeded"
	// CatBadArgument: command line or caller argument error.
	CatBadArgument Category = "bad_argument"
	// CatIO: underlying local file I/O failure.
	CatIO Category = "io_error"
)

// Error is the structured error type used throughout the project.
type Error struct {
	// Category is the machine-readable fault class.
	Category Category
	// TrackID is zero when the fault is not track-scoped.
	TrackID uint32
	// SampleIndex is the 0-based sample locator; -1 when absent.
	SampleIndex int64
	// SeqFirst/SeqLast bracket the RTP sequence numbers involved (mod 65536
	// semantics live in the transport package); -1 when absent.
	SeqFirst int64
	SeqLast  int64
	// Range scopes the fault on the shared nanosecond time base when known.
	Range    clock.TimeRange
	HasRange bool
	// Message is the human-readable explanation.
	Message string
	// Cause allows wrapping an underlying error.
	Cause error
}

// New builds a fault.Error with the given category and message.
func New(cat Category, format string, args ...any) *Error {
	return &Error{
		Category:    cat,
		SampleIndex: -1,
		SeqFirst:    -1,
		SeqLast:     -1,
		Message:     fmt.Sprintf(format, args...),
	}
}

// WithTrack attaches a track locator and returns the receiver.
func (e *Error) WithTrack(trackID uint32) *Error { e.TrackID = trackID; return e }

// WithSample attaches a 0-based sample locator and returns the receiver.
func (e *Error) WithSample(idx int64) *Error { e.SampleIndex = idx; return e }

// WithSeq attaches an RTP sequence-number bracket and returns the receiver.
func (e *Error) WithSeq(first, last int64) *Error {
	e.SeqFirst, e.SeqLast = first, last
	return e
}

// WithTimeRange attaches a nanosecond time-range locator.
func (e *Error) WithTimeRange(tr clock.TimeRange) *Error {
	e.Range, e.HasRange = tr, true
	return e
}

// Wrap records an underlying cause and returns the receiver.
func (e *Error) Wrap(cause error) *Error { e.Cause = cause; return e }

// Error implements the error interface.
func (e *Error) Error() string {
	s := "fault:" + string(e.Category) + ": " + e.Message
	if e.TrackID != 0 {
		s += fmt.Sprintf(" [track=%d]", e.TrackID)
	}
	if e.SampleIndex >= 0 {
		s += fmt.Sprintf(" [sample=%d]", e.SampleIndex)
	}
	if e.SeqFirst >= 0 {
		if e.SeqLast != e.SeqFirst {
			s += fmt.Sprintf(" [seq=%d..%d]", e.SeqFirst, e.SeqLast)
		} else {
			s += fmt.Sprintf(" [seq=%d]", e.SeqFirst)
		}
	}
	if e.HasRange {
		s += fmt.Sprintf(" [time=%d..%dns]", e.Range.StartNanos, e.Range.EndNanos)
	}
	if e.Cause != nil {
		s += ": " + e.Cause.Error()
	}
	return s
}

// Unwrap exposes the wrapped cause to errors.Is/As.
func (e *Error) Unwrap() error { return e.Cause }

// AsError extracts a *fault.Error from any error, wrapping untyped errors
// in CatIO. nil returns nil.
func AsError(err error) *Error {
	if err == nil {
		return nil
	}
	var fe *Error
	if errors.As(err, &fe) {
		return fe
	}
	return New(CatIO, "%s", err.Error())
}

// ExitCode maps a fault category to the process exit code contract:
// 0 success, 2 locatable input damage, 3 recovery/index verification
// failure, 4 bad arguments. I/O and limit failures use 2 as well since
// they manifest as unusable local input.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	fe := AsError(err)
	switch fe.Category {
	case CatBadArgument:
		return 4
	case CatCheckpointMismatch, CatIndexInvalid:
		return 3
	default:
		return 2
	}
}
