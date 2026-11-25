// Package merge restores a single deterministic sample order after parallel
// validation. The order depends only on corrected DTS, container track id and
// the original in-track sequence number — never on which worker happened to
// finish first — so repeated runs yield identical ordering and packet hashes.
package merge

import (
	"sort"

	"mediaarchive/internal/clock"
	"mediaarchive/internal/pipeline"
)

// Less defines the total order used by the archive:
//  1. corrected DTS on the shared timeline (exact rational compare);
//  2. track id (lower first);
//  3. original per-track sample sequence.
func Less(a, b clock.NormalizedSample) bool {
	if c := a.DTSNorm.Cmp(b.DTSNorm); c != 0 {
		return c < 0
	}
	if a.TrackID != b.TrackID {
		return a.TrackID < b.TrackID
	}
	return a.Seq < b.Seq
}

// SortSamples returns the samples sorted by Less. sort.SliceStable keeps
// equal elements in submission order, making the output reproducible even if
// the input slice was produced by an arbitrary number of workers.
func SortSamples(in []clock.NormalizedSample) []clock.NormalizedSample {
	out := make([]clock.NormalizedSample, len(in))
	copy(out, in)
	sort.SliceStable(out, func(i, j int) bool { return Less(out[i], out[j]) })
	return out
}

// SortResults first restores the producer's submission order (using the
// per-job Order assigned single-threaded) and then returns the samples in the
// archive's total order. This two-step sort proves worker-count independence:
// the intermediate buffer can arrive in any completion order.
func SortResults(results []pipeline.Result) []clock.NormalizedSample {
	ordered := make([]pipeline.Result, len(results))
	copy(ordered, results)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Order < ordered[j].Order })
	samps := make([]clock.NormalizedSample, len(ordered))
	for i, r := range ordered {
		samps[i] = r.Sample
	}
	return SortSamples(samps)
}

// IsTotalOrder reports whether the slice already satisfies Less everywhere,
// i.e. whether reordering would be a no-op. Used by tests and checkpoints.
func IsTotalOrder(s []clock.NormalizedSample) bool {
	for i := 1; i < len(s); i++ {
		if Less(s[i], s[i-1]) {
			return false
		}
	}
	return true
}
