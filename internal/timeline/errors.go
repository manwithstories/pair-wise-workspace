package timeline

import (
	"errors"
	"fmt"
)

// Sentinel failures shared by every subsystem. Callers classify failures with
// errors.Is / errors.As rather than parsing strings.
var (
	// ErrInvalidTimebase covers zero denominators and unusable clocks.
	ErrInvalidTimebase = errors.New("invalid timebase")
	// ErrOverflow marks integer wraparound in time math or timestamp expansion.
	ErrOverflow = errors.New("timestamp overflow")
	// ErrNegativeDuration marks end-before-start intervals.
	ErrNegativeDuration = errors.New("negative duration")
	// ErrZeroClock marks a track whose clock never advances.
	ErrZeroClock = errors.New("zero clock")
	// ErrWrap marks an unwrapped timestamp discontinuity handled per track.
	ErrWrap = errors.New("timestamp wraparound")
	// ErrTruncated marks cut-short ID3 / emsg / SEI payloads.
	ErrTruncated = errors.New("truncated metadata payload")
	// ErrCorruptIndex marks an index whose checksum or version does not match.
	ErrCorruptIndex = errors.New("corrupt index")
	// ErrGap marks unrecovered transport packet loss.
	ErrGap = errors.New("unrecovered packet gap")
	// ErrPathEscape marks an output path that leaves the allowed root.
	ErrPathEscape = errors.New("output path escapes root")
	// ErrLimit marks a configured size / track-count limit being exceeded.
	ErrLimit = errors.New("input limit exceeded")
	// ErrCanceled marks a user or context cancellation.
	ErrCanceled = errors.New("canceled")
)

// Error is the uniform structured failure carrying the affected track, the
// raw source timestamp, the normalized microsecond position and whether the
// pipeline can safely resume past the fault without fabricating media data.
type Error struct {
	Op       string   // subsystem operation, e.g. "transport.reorder"
	Track    uint32   // track / stream identifier, 0 when global
	RawTS    int64    // timestamp in the track's native ticks, 0 when unknown
	Timebase Rational // track timebase
	AtMicros int64    // normalized position on the standard scale
	Recoverable bool  // true when safe segments before/after remain usable
	Err      error    // wrapped sentinel / underlying error
}

func (e *Error) Error() string {
	base := e.Err.Error()
	if e.Op != "" {
		base = e.Op + ": " + base
	}
	if e.Track != 0 || e.RawTS != 0 || e.AtMicros != 0 {
		base += fmt.Sprintf(" (track=%d raw_ts=%d at_us=%d)", e.Track, e.RawTS, e.AtMicros)
	}
	if e.Recoverable {
		base += " [recoverable]"
	}
	return base
}

func (e *Error) Unwrap() error { return e.Err }

// Is supports sentinel matching through structured errors.
func (e *Error) Is(target error) bool { return errors.Is(e.Err, target) }

// At returns the normalized microsecond position, if known.
func (e *Error) At() (int64, bool) { return e.AtMicros, e.AtMicros != 0 || e.RawTS != 0 }

// Wrap builds a structured error around an existing failure. A nil err yields
// nil so callers can write return timeline.Wrap(op, track, ..., err) directly.
func Wrap(op string, track uint32, rawTS int64, tb Rational, atMicros int64, recoverable bool, err error) *Error {
	if err == nil {
		return nil
	}
	return &Error{
		Op:          op,
		Track:       track,
		RawTS:       rawTS,
		Timebase:    tb,
		AtMicros:    atMicros,
		Recoverable: recoverable,
		Err:         err,
	}
}

// TrackError is a convenience constructor for track-local failures where the
// normalized position still needs deriving from the raw timestamp.
func TrackError(op string, track uint32, rawTS int64, tb Rational, recoverable bool, err error) *Error {
	at := int64(0)
	if tb.Valid() {
		if v, convErr := ToStandard(rawTS, tb); convErr == nil {
			at = v
		}
	}
	return Wrap(op, track, rawTS, tb, at, recoverable, err)
}

// GlobalError wraps failures not bound to a single track (index, IO, args).
func GlobalError(op string, recoverable bool, err error) *Error {
	return Wrap(op, 0, 0, RationalZero, 0, recoverable, err)
}

// AsError extracts a *timeline.Error from any wrapped error chain.
func AsError(err error) (*Error, bool) {
	var te *Error
	if errors.As(err, &te) {
		return te, true
	}
	return nil, false
}

// Recoverable reports whether the pipeline may continue past this error while
// isolating the affected fragment.
func Recoverable(err error) bool {
	if te, ok := AsError(err); ok {
		return te.Recoverable
	}
	return false
}
