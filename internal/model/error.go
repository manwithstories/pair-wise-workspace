// Package model defines shared data types and the classified error model.
package model

import (
	"errors"
	"fmt"
)

// Kind classifies an error into retryable, fatal or validation categories.
type Kind string

const (
	KindTransient   Kind = "transient"   // retryable: jitter, temporary I/O interruption
	KindCorrupt     Kind = "corrupt"     // input data is damaged; the source segment must be isolated
	KindConflict    Kind = "conflict"    // metadata descriptions cannot be safely reconciled
	KindUnsupported Kind = "unsupported" // layout/rate cannot be handled safely
	KindChecksum    Kind = "checksum"    // hash verification failed
	KindState       Kind = "state"       // journal/transaction state inconsistency
	KindArgument    Kind = "argument"    // bad CLI arguments / path escape
)

// Error is a classified, retryable-aware pipeline error.
type Error struct {
	Kind      Kind
	Stage     string // ingest | rtp | sync | audio | txn | verify
	Op        string // short operation name
	Resource  string
	Detail    string
	Retryable bool
	Cause     error
}

func (e *Error) Error() string {
	base := fmt.Sprintf("%s/%s: %s", e.Stage, e.Op, e.Detail)
	if e.Resource != "" {
		base = e.Resource + ": " + base
	}
	if e.Cause != nil {
		base += ": " + e.Cause.Error()
	}
	return string(e.Kind) + " " + base
}

func (e *Error) Unwrap() error { return e.Cause }

// New builds a classified error.
func New(kind Kind, stage, op, detail string) *Error {
	return &Error{Kind: kind, Stage: stage, Op: op, Detail: detail,
		Retryable: kind == KindTransient}
}

// Wrap annotates an error with resource and cause.
func Wrap(kind Kind, stage, op, resource string, cause error) *Error {
	return &Error{Kind: kind, Stage: stage, Op: op, Resource: resource,
		Detail: cause.Error(), Retryable: kind == KindTransient, Cause: cause}
}

// AsError extracts a *model.Error from any error.
func AsError(err error) (*Error, bool) {
	var me *Error
	if errors.As(err, &me) {
		return me, true
	}
	return nil, false
}

// IsRetryable reports whether the pipeline may retry the failed stage.
func IsRetryable(err error) bool {
	me, ok := AsError(err)
	return ok && me.Retryable
}
