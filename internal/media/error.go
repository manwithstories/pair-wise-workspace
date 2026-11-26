package media

import "fmt"

// ErrorCode classifies every failure the pipeline can produce. The CLI maps
// these onto stable error_code=... tokens and exit codes.
type ErrorCode string

const (
	ErrInvalidTimebase ErrorCode = "INVALID_TIMEBASE"
	ErrManifest        ErrorCode = "MANIFEST_INVALID"
	ErrPathEscape      ErrorCode = "PATH_ESCAPE"
	ErrUnsupported     ErrorCode = "UNSUPPORTED_LAYOUT"
	ErrDemux           ErrorCode = "DEMUX_FAILED"
	ErrCorrupt         ErrorCode = "CORRUPT_STREAM"
	ErrDrift           ErrorCode = "DRIFT_EXCEEDED"
	ErrLate            ErrorCode = "LATE_EVENT_REJECTED"
	ErrCheckpoint      ErrorCode = "CHECKPOINT_FAILED"
	ErrIO              ErrorCode = "IO_FAILED"
	ErrEncode          ErrorCode = "ENCODE_FAILED"
)

// MediaError is the single error type shared by clock, timeline and audio.
// It carries a stable code, a human message and an optional recovery hint.
type MediaError struct {
	Code      ErrorCode
	Message   string
	Recovered bool
	Detail    map[string]string
}

func (e *MediaError) Error() string { return string(e.Code) + ": " + e.Message }

// NewError builds a classified MediaError.
func NewError(code ErrorCode, format string, args ...any) *MediaError {
	return &MediaError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// WithDetail attaches a key/value pair used by diagnostics output.
func (e *MediaError) WithDetail(key, value string) *MediaError {
	if e.Detail == nil {
		e.Detail = map[string]string{}
	}
	e.Detail[key] = value
	return e
}

// AsMediaError extracts a MediaError from an error chain.
func AsMediaError(err error) (*MediaError, bool) {
	for err != nil {
		if me, ok := err.(*MediaError); ok {
			return me, true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			break
		}
		err = u.Unwrap()
	}
	return nil, false
}

// ExitCode maps an error classification onto the CLI exit code contract:
// 0 success, 2 invalid manifest/timebase (non-recoverable input), 3 corrupt
// or otherwise unrecoverable media.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	me, ok := AsMediaError(err)
	if !ok {
		return 3
	}
	switch me.Code {
	case ErrInvalidTimebase, ErrManifest, ErrPathEscape, ErrUnsupported:
		return 2
	default:
		return 3
	}
}
