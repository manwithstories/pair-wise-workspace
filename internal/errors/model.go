// Package errors defines the unified, machine-readable error model shared by
// the scanning, processing and RTP recovery subsystems. Every diagnostic
// carries the track id, the byte offset inside the input and whether the
// condition is recoverable so operators can decide between retry and fallback.
package errors

import (
	stderrors "errors"
	"fmt"
	"strings"
)

// Kind classifies a media routing failure.
type Kind string

const (
	// KindManifest marks an invalid or inconsistent manifest.
	KindManifest Kind = "manifest"
	// KindConfig marks invalid CLI or runtime configuration.
	KindConfig Kind = "config"
	// KindTimeline marks a time base or edit-list inconsistency.
	KindTimeline Kind = "timeline"
	// KindScan marks a sample-table or byte-range read failure.
	KindScan Kind = "scan"
	// KindTruncated marks a sample whose bytes end past the file.
	KindTruncated Kind = "truncated"
	// KindAudio marks an unsupported or failed audio conversion.
	KindAudio Kind = "audio"
	// KindRTP marks an RTP sequence anomaly that could not be recovered.
	KindRTP Kind = "rtp"
	// KindIndex marks a cache/index integrity failure.
	KindIndex Kind = "index"
	// KindCanceled marks a canceled run.
	KindCanceled Kind = "canceled"
	// KindInternal marks an unexpected internal failure.
	KindInternal Kind = "internal"
)

// Error is the unified error model. It intentionally exposes plain fields so
// that the CLI can render deterministic JSON without reflection.
type Error struct {
	Kind        Kind
	TrackID     uint32
	SampleIndex int64 // -1 when not applicable
	ByteOffset  int64 // -1 when not applicable
	Message     string
	Recoverable bool
	Wrapped     error
}

// New builds a recoverable-flagged error.
func New(kind Kind, trackID uint32, sampleIndex, byteOffset int64, recoverable bool, format string, args ...any) *Error {
	return &Error{
		Kind:        kind,
		TrackID:     trackID,
		SampleIndex: sampleIndex,
		ByteOffset:  byteOffset,
		Message:     fmt.Sprintf(format, args...),
		Recoverable: recoverable,
	}
}

// Wrap attaches an underlying cause to an existing model error.
func (e *Error) Wrap(err error) *Error {
	cp := *e
	cp.Wrapped = err
	return &cp
}

// Error implements the error interface.
func (e *Error) Error() string {
	parts := []string{string(e.Kind)}
	if e.TrackID != 0 {
		parts = append(parts, fmt.Sprintf("track=%d", e.TrackID))
	}
	if e.SampleIndex >= 0 {
		parts = append(parts, fmt.Sprintf("sample=%d", e.SampleIndex))
	}
	if e.ByteOffset >= 0 {
		parts = append(parts, fmt.Sprintf("offset=%d", e.ByteOffset))
	}
	if e.Recoverable {
		parts = append(parts, "recoverable")
	}
	msg := strings.Join(parts, " ") + ": " + e.Message
	if e.Wrapped != nil {
		msg += ": " + e.Wrapped.Error()
	}
	return msg
}

// Unwrap exposes the wrapped cause for errors.Is/As.
func (e *Error) Unwrap() error { return e.Wrapped }

// As extracts the unified error model from an error chain.
func As(err error) (*Error, bool) {
	var target *Error
	if stderrors.As(err, &target) {
		return target, true
	}
	return nil, false
}

// ExitCode maps an error to the documented process exit codes:
// 1 = invalid manifest/configuration, 2 = partially recoverable corruption,
// 3 = unexpected internal failure.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	e, ok := As(err)
	if !ok {
		return 3
	}
	switch e.Kind {
	case KindManifest, KindConfig:
		return 1
	case KindCanceled:
		return 3
	case KindInternal:
		return 3
	}
	if e.Recoverable {
		return 2
	}
	return 3
}

// IsRecoverable reports whether the error permits partial output.
func IsRecoverable(err error) bool {
	e, ok := As(err)
	return ok && e.Recoverable
}
