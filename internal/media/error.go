package media

import (
	"errors"
	"fmt"
)

// Code classifies every failure produced by avlab so callers can react
// deterministically instead of inspecting free-form messages.
type Code string

const (
	CodeNone              Code = ""
	CodeInvalidArgument   Code = "invalid_argument"
	CodeManifestDamaged   Code = "manifest_damaged"
	CodeManifestTooLarge  Code = "manifest_too_large"
	CodeBoxOutOfBounds    Code = "box_out_of_bounds"
	CodeBoxTruncated      Code = "box_truncated"
	CodeTimestampOverflow Code = "timestamp_overflow"
	CodeSegmentTooLarge   Code = "segment_too_large"
	CodePacketDuplicate   Code = "packet_duplicate"
	CodePacketLate        Code = "packet_late_epoch"
	CodeGap               Code = "gap"
	CodeDeadlineExceeded  Code = "deadline_exceeded"
	CodeSyncDrift         Code = "sync_drift"
	CodeLimitExceeded     Code = "limit_exceeded"
	CodeCanceled          Code = "canceled"
	CodeInternal          Code = "internal"
)

// Fault is the single error type shared by all modules. It carries enough
// context to be classified, correlated with the timeline and replayed.
type Fault struct {
	Code       Code
	Recoverable bool
	Segment    uint32
	Seq        uint16
	Epoch      Epoch
	Message    string
	Cause      error
}

// Error implements the error interface.
func (f *Fault) Error() string {
	s := string(f.Code)
	if s == "" {
		s = "fault"
	}
	if f.Segment != 0 || f.Seq != 0 {
		s += fmt.Sprintf(" (segment=%d seq=%d epoch=%d)", f.Segment, f.Seq, f.Epoch)
	}
	if f.Message != "" {
		s += ": " + f.Message
	}
	if f.Cause != nil {
		s += ": " + f.Cause.Error()
	}
	return s
}

// Unwrap exposes the wrapped cause for errors.Is/errors.As.
func (f *Fault) Unwrap() error { return f.Cause }

// WithSegment returns a copy annotated with the segment number.
func (f *Fault) WithSegment(seg uint32) *Fault {
	c := *f
	c.Segment = seg
	return &c
}

// WithSeq returns a copy annotated with the RTP sequence number and epoch.
func (f *Fault) WithSeq(seq uint16, ep Epoch) *Fault {
	c := *f
	c.Seq = seq
	c.Epoch = ep
	return &c
}

// NewFault builds a Fault from its static attributes.
func NewFault(code Code, recoverable bool, msg string) *Fault {
	return &Fault{Code: code, Recoverable: recoverable, Message: msg}
}

// WrapFault annotates an underlying error as a Fault.
func WrapFault(code Code, recoverable bool, msg string, cause error) *Fault {
	return &Fault{Code: code, Recoverable: recoverable, Message: msg, Cause: cause}
}

// AsFault extracts a *Fault from err, or returns nil.
func AsFault(err error) *Fault {
	if err == nil {
		return nil
	}
	var f *Fault
	if errors.As(err, &f) {
		return f
	}
	return nil
}

// IsCode reports whether err is a Fault with the given code.
func IsCode(err error, code Code) bool {
	if f := AsFault(err); f != nil {
		return f.Code == code
	}
	return false
}

// Recoverable reports whether the pipeline may continue after err.
func Recoverable(err error) bool {
	if f := AsFault(err); f != nil {
		return f.Recoverable
	}
	return false
}
