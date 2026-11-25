package media

import (
	"fmt"
	"strings"
)

// Level is the recovery grade attached to a fault. It states what the
// pipeline is allowed to do after the fault was observed.
type Level uint8

const (
	// LevelRecovered: the failure was repaired (e.g. XOR recovery) and the
	// affected data still reaches the timeline. The fault is still recorded
	// so reports can distinguish repaired from pristine media.
	LevelRecovered Level = iota + 1
	// LevelDegraded: the affected range cannot be reconstructed; processing
	// continues with an explicit gap. No synthetic media time is invented.
	LevelDegraded
	// LevelFatal: the affected track is stopped at the fault offset; other
	// tracks may continue and diagnostics are still committed.
	LevelFatal
	// LevelAbort: no correct output can be produced. Only the diagnostic
	// text is emitted; no manifest is written.
	LevelAbort
)

func (l Level) String() string {
	switch l {
	case LevelRecovered:
		return "recovered"
	case LevelDegraded:
		return "degraded"
	case LevelFatal:
		return "fatal"
	case LevelAbort:
		return "abort"
	default:
		return "unknown"
	}
}

// Code is a stable machine-readable fault identifier printed by the CLI.
type Code string

const (
	CodeTruncatedElement Code = "truncated_element"
	CodeTruncatedVint    Code = "truncated_vint"
	CodeElementTooLarge  Code = "element_too_large"
	CodeElementTooDeep   Code = "element_too_deep"
	CodeBufferLimit      Code = "buffer_limit"
	CodeBadTrack         Code = "bad_track"
	CodeUnknownTrack     Code = "unknown_track"
	CodeDependencyCycle  Code = "dependency_cycle"
	CodeNonMonotonic     Code = "non_monotonic_timestamp"
	CodeOrphanBlock      Code = "orphan_block"
	CodeJitterOverflow   Code = "jitter_overflow"
	CodeRepairOverflow   Code = "repair_overflow"
	CodeRepairFailed     Code = "repair_failed"
	CodePacketRecovered  Code = "packet_recovered"
	CodePacketGap        Code = "packet_gap"
	CodeSlowConsumer     Code = "slow_consumer"
	CodeCanceled         Code = "canceled"
	CodeTimeout          Code = "timeout"
	CodeDuplicatePacket  Code = "duplicate_packet"
	CodeWriteFailure     Code = "write_failure"
	CodeInternal         Code = "internal"
	CodeSyncAnchor       Code = "sync_anchor_missing"
	CodeSyncMismatch     Code = "sync_mismatch"
)

// Fault is one concrete problem observed by a subsystem. Offset is a byte
// offset for container faults or a packet sequence number for transport
// faults; Track is the affected track number (zero when not applicable).
type Fault struct {
	Level  Level
	Code   Code
	Track  uint64
	Offset int64
	Detail string
}

func (f Fault) String() string {
	var b strings.Builder
	b.WriteString(string(f.Code))
	fmt.Fprintf(&b, "[%s]", f.Level)
	if f.Track != 0 {
		fmt.Fprintf(&b, " track=%d", f.Track)
	}
	if f.Offset != 0 {
		fmt.Fprintf(&b, " offset=%d", f.Offset)
	}
	if f.Detail != "" {
		b.WriteString(": ")
		b.WriteString(f.Detail)
	}
	return b.String()
}
