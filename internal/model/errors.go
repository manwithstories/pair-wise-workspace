// Package model defines the error taxonomy, media events and aggregate
// metrics shared by every package in the media kernel.
package model

import (
	"fmt"

	"mediakernel/internal/clock"
)

// Category classifies every fault observed on the media timeline so that
// downstream tooling can group failures without parsing free-form text.
type Category string

const (
	// CatTruncatedInput covers truncated WAV headers, half frames and short
	// reads that leave a block structure unfulfilled.
	CatTruncatedInput Category = "truncated_input"
	// CatAlignment covers PCM bit depth / sample rate / channel / block
	// alignment validation failures.
	CatAlignment Category = "alignment"
	// CatRTPHeader covers malformed RTP headers that fail parser checks.
	CatRTPHeader Category = "rtp_header"
	// CatDuplicate covers repeated packets carrying identical payloads.
	CatDuplicate Category = "duplicate_packet"
	// CatConflict covers same-sequence packets carrying different payloads.
	CatConflict Category = "sequence_conflict"
	// CatLoss covers detected gaps in per-stream RTP sequence space.
	CatLoss Category = "packet_loss"
	// CatLate covers packets that arrive after the watermark has sealed their
	// time slot and therefore trigger deterministic replay.
	CatLate Category = "late_replay"
	// CatOverflow covers events dropped because bounded buffers were full.
	CatOverflow Category = "buffer_overflow"
	// CatSync covers video timestamp degradation and audio/video drift.
	CatSync Category = "sync"
	// CatUnrecoverable covers loss intervals that the recovery policy cannot
	// reconstruct.
	CatUnrecoverable Category = "unrecoverable"
)

// Decision records how the pipeline reacted to a fault.
type Decision string

const (
	DecRejected      Decision = "rejected"
	DecDeduplicated  Decision = "deduplicated"
	DecIsolated      Decision = "isolated"
	DecBuffered      Decision = "buffered"
	DecReplayed      Decision = "replayed"
	DecSilenceFill   Decision = "silence_filled"
	DecInterpolated  Decision = "interpolated"
	DecDropped       Decision = "dropped_expired"
	DecDegraded      Decision = "degraded"
	DecUnrecoverable Decision = "unrecoverable"
)

// MediaError is a classified, timeline-anchored fault record.
type MediaError struct {
	Category Category
	// Stream is the RTP SSRC/stream identifier, empty for file-level faults.
	Stream uint32
	// At is the event time on the unified clock when the fault belongs.
	At clock.Ticks
	// Seq is the RTP sequence number involved, when applicable.
	Seq uint16
	// Detail is a human-readable, position-bearing explanation.
	Detail string
	// Decision is the recovery action taken.
	Decision Decision
}

// Error implements the error interface.
func (e *MediaError) Error() string {
	if e.Stream != 0 {
		return fmt.Sprintf("%s stream=%d seq=%d at=%s: %s (%s)",
			e.Category, e.Stream, e.Seq, e.At, e.Detail, e.Decision)
	}
	return fmt.Sprintf("%s at=%s: %s (%s)",
		e.Category, e.At, e.Detail, e.Decision)
}

// Log is an append-only, bounded collector of classified faults.
type Log struct {
	items   []MediaError
	limit   int
	evicted int
}

// NewLog creates a fault log capped at limit records. When the cap is reached
// the oldest record is evicted rather than growing without bound; evictions are
// counted separately.
func NewLog(limit int) *Log {
	if limit <= 0 {
		limit = 1024
	}
	return &Log{limit: limit}
}

// Add records a fault.
func (l *Log) Add(e MediaError) {
	if len(l.items) >= l.limit {
		l.items = append(l.items[1:], e)
		l.evicted++
		return
	}
	l.items = append(l.items, e)
}

// Items returns a copy of the recorded faults in append order.
func (l *Log) Items() []MediaError {
	out := make([]MediaError, len(l.items))
	copy(out, l.items)
	return out
}

// CountBy returns how many records of a category exist.
func (l *Log) CountBy(c Category) int {
	n := 0
	for _, e := range l.items {
		if e.Category == c {
			n++
		}
	}
	return n
}

// Evicted reports how many records were dropped due to the log cap.
func (l *Log) Evicted() int { return l.evicted }

// Len returns the number of stored records.
func (l *Log) Len() int { return len(l.items) }
