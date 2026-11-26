package media

import (
	"errors"
	"fmt"
)

// Kind is the stable, machine-readable classification for a pipeline failure.
type Kind string

const (
	KindMalformed  Kind = "malformed"
	KindConflict   Kind = "conflict"
	KindGap        Kind = "gap"
	KindTimeout    Kind = "timeout"
	KindDependency Kind = "dependency"
	KindInternal   Kind = "internal"
)

// Error is the only error shape crossing package boundaries.
type Error struct {
	Kind Kind
	Op   string
	ID   string
	Err  error
}

func (e *Error) Error() string {
	switch {
	case e.ID != "" && e.Err != nil:
		return fmt.Sprintf("%s: %s: %s: %v", e.Kind, e.Op, e.ID, e.Err)
	case e.ID != "":
		return fmt.Sprintf("%s: %s: %s", e.Kind, e.Op, e.ID)
	case e.Err != nil:
		return fmt.Sprintf("%s: %s: %v", e.Kind, e.Op, e.Err)
	default:
		return fmt.Sprintf("%s: %s", e.Kind, e.Op)
	}
}

func (e *Error) Unwrap() error { return e.Err }

// New creates a classified error without a wrapped cause.
func New(kind Kind, op, id string) *Error {
	return &Error{Kind: kind, Op: op, ID: id}
}

// Wrap classifies err. A nil cause returns nil.
func Wrap(kind Kind, op, id string, err error) error {
	if err == nil {
		return nil
	}
	return &Error{Kind: kind, Op: op, ID: id, Err: err}
}

// Classify returns the stable Kind for err. Unknown errors are internal.
func Classify(err error) Kind {
	var qe *Error
	if errors.As(err, &qe) {
		return qe.Kind
	}
	return KindInternal
}

// ExitCode maps classified failures to the terminal contract.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	switch Classify(err) {
	case KindMalformed, KindConflict:
		return 2
	default:
		return 1
	}
}
