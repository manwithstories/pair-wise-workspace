package model

import (
	"errors"
)

// ErrorClass tells the caller what recovery is legal after a failure.
type ErrorClass uint8

const (
	ClassUnknown ErrorClass = iota
	// ClassTransient marks a defect that can be skipped while the committed
	// boundary stays intact (gaps, duplicate packets).
	ClassTransient
	// ClassCorrupt marks malformed input that invalidates the current
	// segment and requires rollback.
	ClassCorrupt
	// ClassCommit marks an interrupted transaction that recovery can replay.
	ClassCommit
	// ClassInvalid marks a caller mistake such as a bad argument.
	ClassInvalid
)

func (c ErrorClass) String() string {
	switch c {
	case ClassTransient:
		return "transient"
	case ClassCorrupt:
		return "corrupt"
	case ClassCommit:
		return "commit"
	case ClassInvalid:
		return "invalid"
	default:
		return "unknown"
	}
}

var (
	ErrInvalid      = errors.New("model: invalid argument")
	ErrTruncated    = errors.New("model: truncated input")
	ErrBadLength    = errors.New("model: malformed box length")
	ErrGap          = errors.New("model: data gap")
	ErrCorruptIndex = errors.New("model: corrupt index")
	ErrRollback     = errors.New("model: rolled back")
	ErrCommit       = errors.New("model: commit interrupted")
	ErrLimit        = errors.New("model: limit exceeded")
)

// Classified pairs an error with its recovery class.
type Classified struct {
	Class ErrorClass
	Err   error
}

func (c *Classified) Error() string { return c.Class.String() + ": " + c.Err.Error() }
func (c *Classified) Unwrap() error { return c.Err }

// Classify wraps err with the given class unless it is already classified.
func Classify(class ErrorClass, err error) error {
	if err == nil {
		return nil
	}
	var existing *Classified
	if errors.As(err, &existing) {
		return err
	}
	return &Classified{Class: class, Err: err}
}

// ClassOf extracts the class of err, defaulting to ClassUnknown.
func ClassOf(err error) ErrorClass {
	var c *Classified
	if errors.As(err, &c) {
		return c.Class
	}
	return ClassUnknown
}
