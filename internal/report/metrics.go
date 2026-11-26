// Package report turns pipeline results into the quantified terminal
// summary. Content-derived fields are deterministic for a given seed; only
// pipeline_p95_ms reflects wall-clock stage timings.
package report

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"audiorestore/internal/model"
)

// Summary is the complete run summary rendered to the terminal.
type Summary struct {
	AVSyncP95US       int64
	LossRecoveryPct   float64
	PipelineP95MS     float64
	Segments          int
	SilenceFrames     int64
	DedupHits         int
	UnrecoveredBlocks []int
	RecoveredBlocks   []int
	StorageIDs        []string
	FingerprintHashes []uint64
	Degraded          bool
	TrackCount        int
	Errors            []model.TrackedError
}

// Percentile returns the p-th percentile (1..100) of durations using the
// nearest-rank method on a sorted copy; zero on empty input.
func Percentile(durations []time.Duration, p int) time.Duration {
	if len(durations) == 0 {
		return 0
	}
	if p < 1 {
		p = 1
	}
	if p > 100 {
		p = 100
	}
	sorted := append([]time.Duration(nil), durations...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	rank := (p*len(sorted) + 99) / 100 // ceil(p*n/100), 1-based
	return sorted[rank-1]
}

// RecoveryPercent computes the recovered share of initially bad blocks.
// No bad blocks at all is reported as 100.0. The division happens once at
// the reporting boundary; all upstream accounting is integer counts.
func RecoveryPercent(recovered, unrecovered int) float64 {
	bad := recovered + unrecovered
	if bad == 0 {
		return 100.0
	}
	return float64(recovered) * 100.0 / float64(bad)
}

// RenderText emits the fixed-field key=value summary. Field order is
// stable and slice contents are sorted so identical inputs produce
// byte-identical output.
func (s *Summary) RenderText() string {
	var b2 strings.Builder
	w := func(k string, v any) { fmt.Fprintf(&b2, "%s=%v\n", k, v) }
	w("av_sync_p95_us", s.AVSyncP95US)
	w("loss_recovery_pct", fmt.Sprintf("%.1f", s.LossRecoveryPct))
	w("pipeline_p95_ms", fmt.Sprintf("%.3f", s.PipelineP95MS))
	w("segments", s.Segments)
	w("silence_frames", s.SilenceFrames)
	w("dedup_hits", s.DedupHits)
	w("unrecovered_blocks", joinInts(sortedCopy(s.UnrecoveredBlocks)))
	w("recovered_blocks", joinInts(sortedCopy(s.RecoveredBlocks)))
	w("degraded", s.Degraded)
	w("track_count", s.TrackCount)
	w("storage_refs", len(s.StorageIDs))
	if len(s.FingerprintHashes) > 0 {
		hashes := append([]uint64(nil), s.FingerprintHashes...)
		sort.Slice(hashes, func(i, j int) bool { return hashes[i] < hashes[j] })
		parts := make([]string, len(hashes))
		for i, h := range hashes {
			parts[i] = fmt.Sprintf("%016x", h)
		}
		w("fingerprints", strings.Join(parts, ","))
	}
	for i, e := range s.Errors {
		fmt.Fprintf(&b2, "error_%d=%s\n", i, e.Error())
	}
	return b2.String()
}

func sortedCopy(in []int) []int {
	out := append([]int(nil), in...)
	sort.Ints(out)
	return out
}

func joinInts(in []int) string {
	if len(in) == 0 {
		return "-"
	}
	parts := make([]string, len(in))
	for i, v := range in {
		parts[i] = fmt.Sprintf("%d", v)
	}
	return strings.Join(parts, ",")
}
