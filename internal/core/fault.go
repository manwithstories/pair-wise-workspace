package core

import (
	"errors"
	"fmt"
)

// Kind is the stable machine-readable category of an archivectl error.
type Kind string

const (
	KindInvalidArgument     Kind = "invalid_argument"
	KindIO                  Kind = "io_error"
	KindMalformedFMT        Kind = "malformed_fmt"
	KindUnsupported         Kind = "unsupported_format"
	KindChunkLength         Kind = "invalid_chunk_length"
	KindPadding             Kind = "missing_padding"
	KindAlignment           Kind = "alignment_error"
	KindClock               Kind = "clock_error"
	KindIndexStale          Kind = "index_stale"
	KindIndexCorrupt        Kind = "index_corrupt"
	KindDamageUnrecoverable Kind = "damage_unrecoverable"
	KindInputRace           Kind = "input_race_changed"
	KindMemoryLimit         Kind = "memory_limit"
	KindVerification        Kind = "verification_failed"
)

// Error is the unified error model shared by all stages.
type Error struct {
	Kind Kind   `json:"kind"`
	Op   string `json:"op"`
	Path string `json:"path,omitempty"`
	Err  error  `json:"-"`
}

func (e *Error) Error() string {
	msg := string(e.Kind)
	if e.Op != "" {
		msg = e.Op + ": " + msg
	}
	if e.Path != "" {
		msg += " [" + e.Path + "]"
	}
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Err }

func New(kind Kind, op, msg string) error {
	return &Error{Kind: kind, Op: op, Err: errors.New(msg)}
}

func Newf(kind Kind, op, format string, args ...any) error {
	return &Error{Kind: kind, Op: op, Err: fmt.Errorf(format, args...)}
}

func Wrap(kind Kind, op, path string, err error) error {
	if err == nil {
		return nil
	}
	return &Error{Kind: kind, Op: op, Path: path, Err: err}
}

// AsKind reports whether err contains an Error of kind.
func AsKind(err error, kind Kind) bool {
	var ce *Error
	if errors.As(err, &ce) {
		return ce.Kind == kind
	}
	return false
}

// Span is an inclusive-start, exclusive-end range in sample frames.
type Span struct {
	Kind        string `json:"kind"`
	StartFrame  int64  `json:"start_frame"`
	EndFrame    int64  `json:"end_frame"`
	Recoverable bool   `json:"recoverable"`
	Source      string `json:"source,omitempty"`
	Detail      string `json:"detail,omitempty"`
}

func (s Span) Frames() int64 {
	if s.EndFrame <= s.StartFrame {
		return 0
	}
	return s.EndFrame - s.StartFrame
}

// MergeSpans combines overlapping or adjacent spans of the same damage kind.
func MergeSpans(spans []Span) []Span {
	if len(spans) == 0 {
		return nil
	}
	out := make([]Span, 0, len(spans))
	for _, s := range spans {
		if s.Frames() <= 0 {
			continue
		}
		merged := false
		for i := range out {
			if out[i].Kind == s.Kind && s.StartFrame <= out[i].EndFrame && out[i].StartFrame <= s.EndFrame {
				if s.StartFrame < out[i].StartFrame {
					out[i].StartFrame = s.StartFrame
				}
				if s.EndFrame > out[i].EndFrame {
					out[i].EndFrame = s.EndFrame
				}
				out[i].Recoverable = out[i].Recoverable && s.Recoverable
				if s.Detail != "" && s.Detail != out[i].Detail {
					out[i].Detail += "; " + s.Detail
				}
				merged = true
				break
			}
		}
		if !merged {
			out = append(out, s)
		}
	}
	return out
}

// ExitCode maps a fatal stage error to the terminal contract.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	if AsKind(err, KindInputRace) {
		return 1
	}
	if AsKind(err, KindDamageUnrecoverable) || AsKind(err, KindMemoryLimit) ||
		AsKind(err, KindMalformedFMT) || AsKind(err, KindUnsupported) {
		return 1
	}
	return 1
}
