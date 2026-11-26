package model

import (
	"errors"
	"fmt"
	"sync"
)

// ErrorCode is a stable machine-readable failure classification.
type ErrorCode string

const (
	CodeInvalidArgument   ErrorCode = "invalid_argument"
	CodeMetadataInvalid   ErrorCode = "metadata_invalid"
	CodeUnknownSource     ErrorCode = "unknown_source"
	CodeDigestMismatch    ErrorCode = "digest_mismatch"
	CodeSourceRead        ErrorCode = "source_read"
	CodeSeqConflict       ErrorCode = "sequence_conflict"
	CodeLatePacket        ErrorCode = "late_packet"
	CodeDuplicatePacket   ErrorCode = "duplicate_packet"
	CodePacketGap         ErrorCode = "packet_gap"
	CodeBurstLoss         ErrorCode = "burst_loss"
	CodeClockRestart      ErrorCode = "clock_restart"
	CodeClockUncertain    ErrorCode = "clock_uncertain"
	CodeSyncGap           ErrorCode = "sync_gap"
	CodeCacheMismatch     ErrorCode = "cache_mismatch"
	CodeCheckpointCorrupt ErrorCode = "checkpoint_corrupt"
	CodeInternal          ErrorCode = "internal"
)

// Exit codes required by the command-line contract.
const (
	ExitOK            = 0
	ExitMediaOrSource = 2
	ExitUsage         = 64
	ExitInternal      = 70
)

// MediaError is the unified error record shared by every pipeline stage.
type MediaError struct {
	Code              ErrorCode `json:"code"`
	TrackID           string    `json:"track_id,omitempty"`
	ExtendedTimestamp uint64    `json:"extended_timestamp,omitempty"`
	SourceByteOffset  int64     `json:"source_byte_offset,omitempty"`
	ExpectedDigest    string    `json:"expected_digest,omitempty"`
	ActualDigest      string    `json:"actual_digest,omitempty"`
	Impact            Interval  `json:"impact,omitempty"`
	Message           string    `json:"message"`
	Recoverable       bool      `json:"recoverable"`
}

func (e *MediaError) Error() string {
	if e == nil {
		return "<nil media error>"
	}
	if e.TrackID != "" {
		return fmt.Sprintf("%s[%s]: %s", e.Code, e.TrackID, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// ExitCode maps a classified error to the process contract.
func ExitCode(err error) int {
	if err == nil {
		return ExitOK
	}
	var me *MediaError
	if errors.As(err, &me) {
		switch me.Code {
		case CodeInvalidArgument:
			return ExitUsage
		case CodeInternal:
			return ExitInternal
		default:
			return ExitMediaOrSource
		}
	}
	return ExitInternal
}

// ErrorBook collects deterministic diagnostics. It is safe for optional
// concurrent producers, although the proof pipeline runs in sequence.
type ErrorBook struct {
	mu    sync.Mutex
	items []*MediaError
}

// Add appends a diagnostic and returns it for convenient wrapping.
func (b *ErrorBook) Add(e *MediaError) *MediaError {
	if e == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.items = append(b.items, e)
	return e
}

// Items returns a copy of collected diagnostics.
func (b *ErrorBook) Items() []*MediaError {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]*MediaError, len(b.items))
	copy(out, b.items)
	return out
}

// Fatal returns the first non-recoverable error.
func (b *ErrorBook) Fatal() *MediaError {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, e := range b.items {
		if e != nil && !e.Recoverable {
			return e
		}
	}
	return nil
}
