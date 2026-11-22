// Package report aggregates quantitative quality results and surfaced errors.
// Every metric is derived from the verified timeline so the numbers printed by
// the CLI can be independently recomputed in tests. Error records contain only
// track ids, offsets and timestamps — never media content.
package report

import (
	"time"

	"mediacq/internal/clock"
)

// QualityError is a located, content-free quality failure.
type QualityError struct {
	Code    string `json:"code"`
	TrackID uint32 `json:"track_id"`
	Offset  int64  `json:"offset"`
	RawTS   int64  `json:"raw_ts"`
	Detail  string `json:"detail"`
}

// Metrics holds the quantitative run result.
type Metrics struct {
	Segments        int            `json:"segments"`
	SilenceSpans    int            `json:"silence_spans"`
	SilenceMS       int64          `json:"silence_ms"`
	Duplicates      int            `json:"duplicates"`
	TargetBPS       int64          `json:"target_bps"`
	ActualBPS       int64          `json:"actual_bps"`
	BitrateDeltaPct float64        `json:"bitrate_delta_pct"`
	AlignmentMS     int64          `json:"manifest_alignment_ms"`
	PeakQueue       int            `json:"peak_queue"`
	QueueCapacity   int            `json:"queue_capacity"`
	DriftWindows    int            `json:"drift_windows"`
	Errors          []QualityError `json:"errors"`
}

// Builder accumulates metrics as stages report in.
type Builder struct {
	m Metrics
}

// NewBuilder starts a report with the configured queue capacity and target.
func NewBuilder(queueCapacity, targetBPS int64) *Builder {
	return &Builder{m: Metrics{QueueCapacity: int(queueCapacity), TargetBPS: targetBPS}}
}

// SetSegments records committed segment count.
func (b *Builder) SetSegments(n int) { b.m.Segments = n }

// AddSilence accumulates a detected silence span.
func (b *Builder) AddSilence(r clock.Range) error {
	d, err := r.Duration()
	if err != nil {
		return err
	}
	b.m.SilenceSpans++
	b.m.SilenceMS += d.Milliseconds()
	return nil
}

// SetDuplicates records the deduper's duplicate count.
func (b *Builder) SetDuplicates(n int) { b.m.Duplicates = n }

// SetBitrate computes actual bitrate from total payload bytes and the program
// duration, then the signed percentage delta against the target.
func (b *Builder) SetBitrate(totalBytes int, dur time.Duration) {
	if dur <= 0 {
		b.m.ActualBPS = 0
		b.m.BitrateDeltaPct = 0
		return
	}
	b.m.ActualBPS = int64(float64(totalBytes) * 8 / dur.Seconds())
	if b.m.TargetBPS > 0 {
		b.m.BitrateDeltaPct = (float64(b.m.ActualBPS-b.m.TargetBPS) / float64(b.m.TargetBPS)) * 100
	}
}

// SetAlignment records the worst manifest alignment error in milliseconds.
func (b *Builder) SetAlignment(ms int64) { b.m.AlignmentMS = ms }

// SetPeakQueue records the observed maximum queue depth.
func (b *Builder) SetPeakQueue(n int) { b.m.PeakQueue = n }

// AddDriftWindow counts an over-threshold drift region.
func (b *Builder) AddDriftWindow() { b.m.DriftWindows++ }

// AddError records a content-free located error and counts it.
func (b *Builder) AddError(e QualityError) { b.m.Errors = append(b.m.Errors, e) }

// QualityErrors returns the number of recorded errors.
func (b *Builder) QualityErrors() int { return len(b.m.Errors) }

// Snapshot returns a value copy of the accumulated metrics.
func (b *Builder) Snapshot() Metrics {
	cp := b.m
	cp.Errors = append([]QualityError(nil), b.m.Errors...)
	return cp
}

// AssertBitrate fails (returns false) when the absolute bitrate delta exceeds
// the allowed percentage.
func (m Metrics) AssertBitrate(tolerancePct float64) bool {
	d := m.BitrateDeltaPct
	if d < 0 {
		d = -d
	}
	return d <= tolerancePct
}

// AssertAlignment fails when the worst alignment error is over the limit.
func (m Metrics) AssertAlignment(maxMS int64) bool { return m.AlignmentMS <= maxMS }
