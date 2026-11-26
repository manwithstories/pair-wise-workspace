package model

import "fmt"

// Stage identifies the subsystem that raised an error.
type Stage string

const (
	StageTimecode Stage = "timecode"
	StageCue      Stage = "cue"
	StagePipe     Stage = "pipe"
	StageHLS      Stage = "hls"
	StageMetrics  Stage = "metrics"
)

// Error carries the stage, source line, media time and a recoverable
// marker shared by all subsystems. Recoverable errors are reported and
// skipped; non-recoverable ones abort the run with exit code 1.
type Error struct {
	Stage       Stage
	Line        int   // 1-based source line, 0 when not applicable
	MediaMs     int64 // media time context, -1 when unknown
	Recoverable bool
	Msg         string
	Err         error
}

func (e *Error) Error() string {
	s := fmt.Sprintf("[%s] %s", e.Stage, e.Msg)
	if e.Line > 0 {
		s += fmt.Sprintf(" (line %d)", e.Line)
	}
	if e.MediaMs >= 0 {
		s += fmt.Sprintf(" (media_ms %d)", e.MediaMs)
	}
	if e.Err != nil {
		s += ": " + e.Err.Error()
	}
	return s
}

func (e *Error) Unwrap() error { return e.Err }

// Recoverablef builds a recoverable stage error.
func Recoverablef(stage Stage, line int, mediaMs int64, format string, args ...any) *Error {
	return &Error{Stage: stage, Line: line, MediaMs: mediaMs, Recoverable: true, Msg: fmt.Sprintf(format, args...)}
}

// Fatalf builds a non-recoverable stage error.
func Fatalf(stage Stage, format string, args ...any) *Error {
	return &Error{Stage: stage, MediaMs: -1, Msg: fmt.Sprintf(format, args...)}
}
