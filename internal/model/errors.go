// Package model defines the error vocabulary and timeline value objects
// shared by the probe, segment, index, rtp and pipeline packages.
//
// Error codes are strings rather than Go error types so that probe defects,
// manifest gaps and RTP unrecoverable ranges can be persisted in checkpoints
// and manifests with one stable, byte-stable vocabulary.
package model

import (
	"errors"
	"fmt"
)

// ErrorCode is the cross-module failure vocabulary.
type ErrorCode string

const (
	// CodeOK means no defect was recorded for the range.
	CodeOK ErrorCode = "ok"

	// CodeContainerTruncated: declared boxes run past end of file. The
	// recoverable prefix stays in the manifest; the tail is quarantined.
	CodeContainerTruncated ErrorCode = "container_truncated"

	// CodeCorruptSample: a declared sample is unreadable or fails its
	// integrity check inside an otherwise intact container.
	CodeCorruptSample ErrorCode = "corrupt_sample"

	// CodeIndexMissingPage: the sample table/index is incomplete, so sample
	// boundaries after the last valid page cannot be verified.
	CodeIndexMissingPage ErrorCode = "index_missing_page"

	// CodeLengthOutOfBounds: a declared box/sample length crosses its
	// parent boundary; the parser must never read past that boundary.
	CodeLengthOutOfBounds ErrorCode = "length_out_of_bounds"

	// CodeNoDecodableBoundary: no keyframe exists within the bounded search
	// window around a planned cut; the range is a gap, never a fake segment.
	CodeNoDecodableBoundary ErrorCode = "no_decodable_boundary"

	// CodeRepresentationDrift: representations disagree on a shared cut by
	// more than the bounded fallback allowance.
	CodeRepresentationDrift ErrorCode = "representation_drift"

	// CodeRTPUnrecoverable: a sequence-number window cannot be reassembled
	// because media packets (not just redundancy) are missing beyond repair.
	CodeRTPUnrecoverable ErrorCode = "rtp_unrecoverable"

	// CodeCheckpointMismatch: checkpoint input digest differs from the
	// current input; resume is refused rather than mixing two inputs.
	CodeCheckpointMismatch ErrorCode = "checkpoint_mismatch"

	// CodeIndexWriteFailed: atomic index/manifest write failed.
	CodeIndexWriteFailed ErrorCode = "index_write_failed"

	// CodeCanceled: processing was interrupted; resumable by design.
	CodeCanceled ErrorCode = "canceled"
)

// Recoverable reports whether processing of later ranges may continue after
// a defect carrying this code. Structural write failures and digest
// mismatches are not recoverable on the same state.
func (c ErrorCode) Recoverable() bool {
	switch c {
	case CodeContainerTruncated, CodeCorruptSample, CodeIndexMissingPage,
		CodeLengthOutOfBounds, CodeNoDecodableBoundary,
		CodeRepresentationDrift, CodeRTPUnrecoverable, CodeCanceled:
		return true
	default:
		return false
	}
}

// MediaError carries a code with the tick range it applies to. Ranges use
// the shared clock (see internal/clock); zero-length ranges mark a point.
type MediaError struct {
	Code    ErrorCode `json:"code"`
	Start   int64     `json:"start_ticks"`
	End     int64     `json:"end_ticks"`
	Detail  string    `json:"detail,omitempty"`
	Source  string    `json:"source"`
	Resumed bool      `json:"resumed,omitempty"`
}

// Error implements error.
func (e *MediaError) Error() string {
	return fmt.Sprintf("%s: ticks [%d,%d) source=%s: %s",
		e.Code, e.Start, e.End, e.Source, e.Detail)
}

// Is lets errors.Is match by code against a sentinel.
func (e *MediaError) Is(target error) bool {
	var me *MediaError
	if errors.As(target, &me) {
		return me.Code == e.Code
	}
	return false
}

// NewError builds a range-annotated media error.
func NewError(code ErrorCode, source string, start, end int64, detail string) *MediaError {
	return &MediaError{Code: code, Source: source, Start: start, End: end, Detail: detail}
}

// TimeRange is a half-open interval [Start, End) on the shared timeline.
type TimeRange struct {
	Start int64 `json:"start_ticks"`
	End   int64 `json:"end_ticks"`
}

// Length returns the span in shared ticks.
func (r TimeRange) Length() int64 { return r.End - r.Start }

// Contains reports whether t falls inside the half-open interval.
func (r TimeRange) Contains(t int64) bool { return t >= r.Start && t < r.End }

// Overlaps reports whether two half-open intervals share any tick.
func (r TimeRange) Overlaps(o TimeRange) bool {
	return r.Start < o.End && o.Start < r.End
}

// SegmentState is the recovery lifecycle of a manifest entry.
type SegmentState string

const (
	// StatePlanned: cut points chosen, not yet processed.
	StatePlanned SegmentState = "planned"
	// StateCommitted: chunk verified and durably recorded exactly once.
	StateCommitted SegmentState = "committed"
	// StateQuarantined: unverifiable tail/gap; excluded from output segments.
	StateQuarantined SegmentState = "quarantined"
)

// SourceRef keeps a verifiable provenance pointer for every produced range:
// which input, which track, and the byte span the data came from.
type SourceRef struct {
	InputID     string    `json:"input_id"`
	TrackID     uint32    `json:"track_id"`
	ByteOffset  int64     `json:"byte_offset"`
	ByteLength  int64     `json:"byte_length"`
	SampleRange [2]uint32 `json:"sample_range"` // 1-based, half-open
}
