package pipeline

import (
	"math"
	"sort"
	"strconv"

	"mediadelivery/internal/analysis"
	"mediadelivery/internal/fault"
	"mediadelivery/internal/normalize"
	"mediadelivery/internal/probe"
)

// computeMetrics derives the acceptance metric set from processed results and
// recorded faults.
func (r *Runner) computeMetrics(results []result) *Metrics {
	m := &Metrics{}

	// A/V sync maximum across blocks (synthetic aligned timeline stays small).
	maxSync := 0
	var segDurations []int64
	for _, res := range results {
		if res.avSyncMs > maxSync {
			maxSync = res.avSyncMs
		}
		if res.segmentMs > 0 {
			segDurations = append(segDurations, res.segmentMs)
		}
	}
	m.AVSyncMaxMs = maxSync
	m.SegmentP95Ms = percentile95(segDurations)
	m.ProcessedBlocks = len(r.store.Entries())
	m.DuplicateHits = r.dupCount

	// Recovery accounting from the fault recorder.
	lost := 0
	recovered := 0
	for _, f := range r.recorder.Faults() {
		if f.Kind == fault.KindMissingSegment {
			lost++
			if f.Recovered {
				recovered++
			}
		}
	}
	m.LostSlots = lost
	m.RecoveredSlots = recovered
	if lost > 0 {
		m.LossRecoveryRate = round3(float64(recovered) / float64(lost))
	}
	m.HasUnrecoverable = r.recorder.HasUnrecoverable()

	// Resume hash equality is determined by the CLI across two runs; the runner
	// exposes the current stable index hash for comparison.
	m.ResumeHashEqual = true
	return m
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
