package app

import (
	"fmt"
	"time"

	"mediaproc/internal/model"
)

// Options configures the terminal demo adapter.
type Options struct {
	Seed         int64
	Out          string
	MaxMemory    int64
	Queue        int
	SlowConsumer time.Duration
	Fault        string
	Recover      bool
	CancelAfter  time.Duration
}

// Report is the deterministic processing result and terminal counters.
type Report struct {
	Segments         int     `json:"segments"`
	HashOK           int     `json:"hash_ok"`
	HashTotal        int     `json:"hash_total"`
	RoundtripPercent string  `json:"roundtrip"`
	KeyframePercent  string  `json:"keyframe_hit"`
	Silence          int     `json:"silence"`
	Dedup            int     `json:"dedup"`
	PeakQueue        int     `json:"peak_queue"`
	PeakMemory       int64   `json:"peak_memory"`
	MemoryLimit      int64   `json:"memory_limit"`
	Recoveries       int     `json:"recoveries"`
	Cancelled        int     `json:"cancelled"`
	FirstErrorOffset int64   `json:"first_error_offset,omitempty"`
	RecoveryOffsets  []int64 `json:"recovery_offsets,omitempty"`
	TrackErrors      int     `json:"track_errors"`
	RTPPackets       int     `json:"rtp_packets"`
	ExitCode         int     `json:"exit_code"`
}

// NormalSummary returns the required happy-path terminal line.
func (r Report) NormalSummary() string {
	return fmt.Sprintf(
		"roundtrip=%s keyframe_hit=%s segments=%d hash_ok=%d/%d silence=%d dedup=%d peak_queue=%d peak_memory=%d peak_memory<=%d",
		r.RoundtripPercent, r.KeyframePercent, r.Segments, r.HashOK, r.HashTotal,
		r.Silence, r.Dedup, r.PeakQueue, r.PeakMemory, r.MemoryLimit,
	)
}

// RecoverySummary returns the required corruption-recovery line.
func (r Report) RecoverySummary() string {
	return fmt.Sprintf(
		"recoveries=%d first_error_offset=%d segments=%d hash_ok=%d/%d",
		r.Recoveries, r.FirstErrorOffset, r.Segments, r.HashOK, r.HashTotal,
	)
}

// CancelSummary reports full-queue cancellation and cleanup.
func (r Report) CancelSummary() string {
	return fmt.Sprintf(
		"queue_cancel=1 first_error_offset=%d cleaned=%d peak_queue=%d peak_memory<=%d",
		r.FirstErrorOffset, r.Cancelled, r.PeakQueue, r.MemoryLimit,
	)
}

// MemoryOK reports whether observed queue memory stayed in budget.
func (r Report) MemoryOK() bool { return r.PeakMemory <= r.MemoryLimit }

// ExitCodeFor maps a processing error to the stable CLI exit code.
func ExitCodeFor(err error) int { return model.ExitCode(err) }
