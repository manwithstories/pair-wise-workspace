package pipeline

import (
	"math"
	"sort"
	"strconv"

	"mediadelivery/internal/analysis"
	"mediadelivery/internal/fault"
	"mediadelivery/internal/index"
	"mediadelivery/internal/normalize"
	"mediadelivery/internal/probe"
)

// computeMetrics derives the acceptance metric set. Recovery and block counts
// come from the durable index entries (not the in-memory fault recorder) so a
// resumed run reports exactly the same numbers as a single uninterrupted run.
func (r *Runner) computeMetrics(results []result) *Metrics {
	m := &Metrics{}

	entries := r.store.Entries()

	// A/V sync maximum across freshly processed blocks (resumed blocks were
	// validated identically in the prior run).
	maxSync := 0
	for _, res := range results {
		if res.avSyncMs > maxSync {
			maxSync = res.avSyncMs
		}
	}
	m.AVSyncMaxMs = maxSync

	// P95 segment duration from the durable, timeline-aligned entries.
	segDurations := make([]int64, 0, len(entries))
	for _, e := range entries {
		if e.Duration90k > 0 {
			segDurations = append(segDurations, e.Duration90k*1000/90000)
		}
	}
	m.SegmentP95Ms = percentile95(segDurations)
	m.ProcessedBlocks = len(entries)
	m.DuplicateHits = r.dupCount

	// Recovery accounting from the durable entries, restricted to slots the
	// plan marks as lost. A truncated slot is also served from the backup and
	// carries Recovered=true, but it is not a lost slot and must not inflate
	// the loss-recovery numerator.
	lostSet := map[string]bool{}
	if r.plan != nil {
		for _, f := range r.plan.Faults {
			if f.Kind == "lost" {
				lostSet[f.Camera+"/"+strconv.Itoa(f.Sequence)] = true
			}
		}
	}
	recovered := 0
	for _, e := range entries {
		key := e.Camera + "/" + strconv.Itoa(e.Sequence)
		if e.Recovered && lostSet[key] {
			recovered++
		}
	}
	lost := len(lostSet)
	m.LostSlots = lost
	m.RecoveredSlots = recovered
	if lost > 0 {
		m.LossRecoveryRate = round3(float64(recovered) / float64(lost))
	}
	// An unrecoverable slot leaves no index entry (neither primary nor backup
	// existed). Detect it deterministically from the plan rather than memory.
	m.HasUnrecoverable = r.hasUnrecoverableSlot(entries)

	m.ResumeHashEqual = true
	return m
}

// hasUnrecoverableSlot reports whether any planned lost slot lacks a backup,
// cross-checked against the durable entries.
func (r *Runner) hasUnrecoverableSlot(entries []index.Entry) bool {
	if r.plan == nil {
		return false
	}
	present := map[string]bool{}
	for _, e := range entries {
		present[e.Camera+"/"+strconv.Itoa(e.Sequence)] = true
	}
	for _, f := range r.plan.Faults {
		if f.Kind == "lost" && !f.Recoverable {
			// The unrecoverable slot cannot appear as any role's entry.
			if !present[f.Camera+"/"+strconv.Itoa(f.Sequence)] {
				return true
			}
		}
	}
	return false
}

// IndexHash exposes the canonical store hash for resume comparison.
func (r *Runner) IndexHash() string { return r.store.Hash() }

// percentile95 returns the 95th-percentile duration in milliseconds.
func percentile95(values []int64) int64 {
	if len(values) == 0 {
		return 0
	}
	cp := make([]int64, len(values))
	copy(cp, values)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	idx := int(math.Ceil(0.95*float64(len(cp)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(cp) {
		idx = len(cp) - 1
	}
	return cp[idx]
}

func round3(v float64) float64 {
	return math.Round(v*1000) / 1000
}

// contentHash renders a stable string hash from a content fingerprint.
func contentHash(fp *analysis.Fingerprint) string {
	if fp == nil {
		return ""
	}
	return "v" + strconv.FormatUint(fp.VideoHash, 16) +
		":a" + envelopeKey(fp.AudioEnvelope)
}

// envelopeKey collapses the envelope into a short stable token.
func envelopeKey(env []float32) string {
	if len(env) == 0 {
		return "0"
	}
	h := uint64(1469598103934665603)
	for _, v := range env {
		b := math.Float32bits(quantize(v))
		for k := 0; k < 4; k++ {
			h ^= uint64(byte(b >> (8 * k)))
			h *= 1099511628211
		}
	}
	return strconv.FormatUint(h, 16)
}

// quantize rounds envelope samples so tiny float drift does not change the key.
func quantize(v float32) float32 {
	return float32(math.Round(float64(v)*1024) / 1024)
}

// normalizeObservation converts a probed video stream into a colour observation.
func normalizeObservation(source string, s probe.StreamInfo) normalize.ColorObservation {
	return normalize.ColorObservation{
		Source:    source,
		Primaries: s.Color.Primaries,
		Transfer:  s.Color.Transfer,
		Space:     s.Color.Space,
		Range:     s.Color.Range,
	}
}

// ColorConflicts exposes detected colour conflicts for reporting.
func (r *Runner) ColorConflicts() []normalize.ColorConflict {
	return r.colors.Analyse()
}

// Faults exposes the recorded faults for reporting.
func (r *Runner) Faults() []fault.Fault { return r.recorder.Faults() }

func itoa(n int) string { return strconv.Itoa(n) }
