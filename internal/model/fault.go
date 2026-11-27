package model

import "fmt"

// FaultKind classifies every anomaly reported across modules so that
// downstream stages can reason about trust uniformly.
type FaultKind int

const (
	FaultNone FaultKind = iota
	FaultSyncLoss        // 188-byte sync boundary violated
	FaultContinuityGap   // continuity counter jumped forward
	FaultDuplicatePacket // repeated continuity counter (not an error)
	FaultPSICRC          // PSI section CRC mismatch
	FaultPSIVersion      // illegal PSI version transition
	FaultTruncated       // trailing partial packet / truncated input
	FaultClockWrap       // 33-bit timestamp wrap (handled by unwrap)
	FaultUnanchoredEpoch // time epoch without a trusted PCR anchor
	FaultIncompletePES   // PES ended before its declared length
	FaultOversizePES     // PES declared length exceeds the buffer limit
	FaultInvalidTS       // timestamp failed validity checks
	FaultBoundary        // keyframe boundary conflict across branches
)

// Fault is a single classified anomaly tied to a byte offset in the input.
type Fault struct {
	Kind   FaultKind
	Offset int64 // byte offset in the source stream, -1 if not applicable
	PID    int   // affected PID, -1 if not applicable
	Detail string
}

func (f Fault) Error() string {
	return fmt.Sprintf("fault %s at offset %d (pid %d): %s", f.Kind, f.Offset, f.PID, f.Detail)
}

// Recoverable reports whether processing may continue past this fault kind
// using the last trusted state.
func (k FaultKind) Recoverable() bool {
	switch k {
	case FaultUnanchoredEpoch, FaultBoundary:
		return false
	default:
		return true
	}
}

func (k FaultKind) String() string {
	switch k {
	case FaultNone:
		return "none"
	case FaultSyncLoss:
		return "sync_loss"
	case FaultContinuityGap:
		return "continuity_gap"
	case FaultDuplicatePacket:
		return "duplicate_packet"
	case FaultPSICRC:
		return "psi_crc"
	case FaultPSIVersion:
		return "psi_version"
	case FaultTruncated:
		return "truncated"
	case FaultClockWrap:
		return "clock_wrap"
	case FaultUnanchoredEpoch:
		return "unanchored_epoch"
	case FaultIncompletePES:
		return "incomplete_pes"
	case FaultOversizePES:
		return "oversize_pes"
	case FaultInvalidTS:
		return "invalid_timestamp"
	case FaultBoundary:
		return "boundary_conflict"
	default:
		return "unknown"
	}
}
