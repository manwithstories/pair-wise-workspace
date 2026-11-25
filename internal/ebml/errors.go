package ebml

import (
	"errors"
	"fmt"
	"io"

	"avcheck/internal/media"
)

// ErrTruncated is the generic unexpected-end marker; the two specialised
// errors below let the pipeline distinguish a cut vint from a cut element
// body when it reports the fault offset.
var (
	ErrTruncated        = errors.New("ebml: unexpected end of stream")
	ErrTruncatedVint    = errors.New("ebml: truncated variable-length integer")
	ErrTruncatedElement = errors.New("ebml: truncated element body")
)

// ErrTooLarge marks input promising more data than a configured cap allows.
var ErrTooLarge = errors.New("ebml: declared size exceeds configured limit")

// ErrBadVint marks an invalid variable length integer (zero first byte).
var ErrBadVint = errors.New("ebml: invalid vint")

// ErrDepth marks nesting deeper than the configured cap.
var ErrDepth = errors.New("ebml: nesting depth exceeds configured limit")

// posError attaches a byte offset to an error so faultFor can report the
// exact site of a truncation.
type posError struct {
	err error
	off int64
}

func (e *posError) Error() string { return fmt.Sprintf("%v at offset %d", e.err, e.off) }
func (e *posError) Unwrap() error { return e.err }
func (e *posError) Offset() int64 { return e.off }

func isEOF(err error) bool {
	return errors.Is(err, io.EOF)
}

// faultFor converts a cursor error into a shared media fault at the given
// element header offset.
func faultFor(err error, off int64, track uint64) media.Fault {
	if off == 0 {
		if o, ok := err.(interface{ Offset() int64 }); ok {
			off = o.Offset()
		}
	}
	switch {
	case errors.Is(err, ErrTooLarge):
		return media.Fault{Level: media.LevelFatal, Code: media.CodeElementTooLarge, Track: track, Offset: off, Detail: err.Error()}
	case errors.Is(err, ErrTruncatedVint):
		return media.Fault{Level: media.LevelAbort, Code: media.CodeTruncatedVint, Track: track, Offset: off, Detail: err.Error()}
	case errors.Is(err, ErrTruncatedElement), errors.Is(err, ErrTruncated):
		return media.Fault{Level: media.LevelAbort, Code: media.CodeTruncatedElement, Track: track, Offset: off, Detail: err.Error()}
	case errors.Is(err, ErrDepth):
		return media.Fault{Level: media.LevelFatal, Code: media.CodeElementTooDeep, Track: track, Offset: off, Detail: err.Error()}
	case errors.Is(err, ErrBadVint):
		return media.Fault{Level: media.LevelFatal, Code: media.CodeBadTrack, Track: track, Offset: off, Detail: err.Error()}
	default:
		return media.Fault{Level: media.LevelAbort, Code: media.CodeInternal, Track: track, Offset: off, Detail: err.Error()}
	}
}
