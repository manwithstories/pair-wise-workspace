// Package model defines the data types shared by every mediad subsystem and
// the single error model they all report through.
package model

import (
	"errors"
	"fmt"
)

// Kind classifies an error so the CLI can map it to the documented exit code.
type Kind int

const (
	// KindInternal means an unexpected local failure.
	KindInternal Kind = iota
	// KindInvalidInput means the caller supplied unusable arguments.
	KindInvalidInput
	// KindInvalidMedia means the media sample or its metadata is not usable.
	KindInvalidMedia
	// KindRecovery means a WAL, checkpoint or commit could not be recovered.
	KindRecovery
	// KindThreshold means processing succeeded but a quantitative gate failed.
	KindThreshold
	// KindOverLimit means a configured memory or reorder limit was exceeded.
	KindOverLimit
)

// String returns a stable, lowercase identifier.
func (k Kind) String() string {
	switch k {
	case KindInvalidInput:
		return "invalid_input"
	case KindInvalidMedia:
		return "invalid_media"
	case KindRecovery:
		return "recovery"
	case KindThreshold:
		return "threshold"
	case KindOverLimit:
		return "over_limit"
	default:
		return "internal"
	}
}

// ExitCode maps a Kind to the exit code documented for the CLI.
func (k Kind) ExitCode() int {
	switch k {
	case KindInvalidInput, KindInvalidMedia, KindOverLimit:
		return 2
	case KindRecovery:
		return 3
	case KindThreshold:
		return 4
	default:
		return 1
	}
}

// Error is the unified error value every subsystem returns. It carries the
// stage, the affected track (when known) and the underlying cause so that
// terminal output can explain *why* a frame moved or an index was rebuilt.
type Error struct {
	Kind    Kind
	Stage   string
	TrackID uint32
	Detail  string
	Err     error
}

// New builds an Error without an underlying cause.
func New(k Kind, stage, detail string) *Error {
	return &Error{Kind: k, Stage: stage, Detail: detail}
}

// Wrap builds an Error around cause.
func Wrap(k Kind, stage, detail string, cause error) *Error {
	return &Error{Kind: k, Stage: stage, Detail: detail, Err: cause}
}

// WithTrack returns a copy tagged with a track identifier.
func (e *Error) WithTrack(id uint32) *Error {
	c := *e
	c.TrackID = id
	return &c
}

// ExitCode maps this error's Kind to its documented process exit code.
func (e *Error) ExitCode() int { return e.Kind.ExitCode() }

// Error implements error.
func (e *Error) Error() string {
	s := e.Kind.String() + ": " + e.Stage
	if e.TrackID != 0 {
		s += fmt.Sprintf(": track=%d", e.TrackID)
	}
	if e.Detail != "" {
		s += ": " + e.Detail
	}
	if e.Err != nil {
		s += ": " + e.Err.Error()
	}
	return s
}

// Unwrap exposes the cause.
func (e *Error) Unwrap() error { return e.Err }

// KindOf extracts the Kind from any error in the chain, defaulting to KindInternal.
func KindOf(err error) Kind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return KindInternal
}

// ExitCodeOf returns the exit code for any error.
func ExitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	return KindOf(err).ExitCode()
}

// AsError finds the unified Error in the chain.
func AsError(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}
