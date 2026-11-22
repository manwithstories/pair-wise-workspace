// Package fault defines the shared error model with recovery levels.
package fault

import (
	"errors"
	"fmt"
)

// Level classifies how a failure affects the batch.
type Level int

const (
	// Recoverable anomalies are logged; processing continues.
	Recoverable Level = iota
	// Degraded keeps partial output but marks it suspect.
	Degraded
	// Fatal marks an unrecoverable dependency; the batch stops.
	Fatal
)

func (l Level) String() string {
	switch l {
	case Recoverable:
		return "recoverable"
	case Degraded:
		return "degraded"
	default:
		return "fatal"
	}
}

// Error carries a recovery level plus operation and path context.
type Error struct {
	Level Level
	Op    string
	Path  string
	Err   error
}

func (e *Error) Error() string {
	if e.Path != "" {
		return e.Op + " " + e.Path + ": " + e.Err.Error()
	}
	return e.Op + ": " + e.Err.Error()
}

func (e *Error) Unwrap() error { return e.Err }

// Wrap annotates err with a level, operation and path.
func Wrap(level Level, op, path string, err error) *Error {
	return &Error{Level: level, Op: op, Path: path, Err: err}
}

// Newf creates a levelled error from a formatted message.
func Newf(level Level, op, path, format string, args ...interface{}) *Error {
	return Wrap(level, op, path, fmt.Errorf(format, args...))
}

// LevelOf extracts the level of err, defaulting to Fatal.
func LevelOf(err error) Level {
	var e *Error
	if errors.As(err, &e) {
		return e.Level
	}
	return Fatal
}
