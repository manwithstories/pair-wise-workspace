package pipeline

import (
	"sort"

	"mediamix/internal/core"
	"mediamix/internal/timeline"
	"mediamix/internal/transport"
)

// accumulate folds per-track results into the run-level metrics. Video tracks
// drive sync error; all tracks contribute to the latency distribution; failed
// tracks are excluded from recovery and latency so they cannot skew healthy
// results.
func (r *Report) accumulate(results map[string]*TrackResult, engine *transport.RecoveryEngine) {
	var latencies []float64
	var syncOffsets []timeline.Micros
	for _, tr := range r.Tracks {
		switch tr.Kind {
		case timeline.KindVideo:
			if tr.Status != StatusFailed {
				syncOffsets = append(syncOffsets, tr.SyncOffsetUs)
			}
		}
		r.ReorderedPackets += tr.Reordered
		r.DuplicatePackets += tr.Duplicates
		if tr.Status != StatusFailed {
			latencies = append(latencies, tr.LatencyP95Ms)
		}
		if tr.Status == StatusFailed {
			r.FailedTracks = append(r.FailedTracks, tr.Track)
		}
		if tr.Status == StatusDegraded {
			r.DegradedTracks = append(r.DegradedTracks, tr.Track)
		}
		if tr.Unrecoverable > 0 {
			r.Unrecoverable = append(r.Unrecoverable, tr.Track)
		}
	}
	sort.Strings(r.FailedTracks)
	sort.Strings(r.DegradedTracks)
	sort.Strings(r.Unrecoverable)

	// Sync error is the presentation spread between healthy tracks: the gap
	// between the earliest and latest effective origin. A/V drift shows up here
	// as soon as one track's media becomes available later than another's.
	if len(syncOffsets) > 1 {
		lo, hi := syncOffsets[0], syncOffsets[0]
		for _, off := range syncOffsets[1:] {
			if off < lo {
				lo = off
			}
			if off > hi {
				hi = off
			}
		}
		r.SyncErrorMS = float64(hi-lo) / 1000
	}
	sort.Float64s(latencies)
	r.LatencyP95MS = percentile(latencies, 0.95)
	r.RecoveryRate = engine.RecoveryRate()
}

// finalizeCollections replaces nil slices with empty ones so the JSON contract
// always emits arrays for list fields.
func (r *Report) finalizeCollections() {
	if r.FailedTracks == nil {
		r.FailedTracks = []string{}
	}
	if r.DegradedTracks == nil {
		r.DegradedTracks = []string{}
	}
	if r.Unrecoverable == nil {
		r.Unrecoverable = []string{}
	}
	if r.ValidationErrors == nil {
		r.ValidationErrors = []*core.Error{}
	}
	if r.Tracks == nil {
		r.Tracks = []TrackResult{}
	}
}

// finish decides the exit code and a final consistency pass.
func (r *Report) finish() {
	sort.SliceStable(r.Tracks, func(i, j int) bool { return r.Tracks[i].Track < r.Tracks[j].Track })
	// Surface per-track index failures as top-level validation errors so the
	// CLI can report "index error, affected track" without digging into tracks.
	for _, tr := range r.Tracks {
		if tr.Status == StatusFailed && tr.Error != nil && tr.Error.Code == core.CodeBadIndex {
			r.ValidationErrors = append(r.ValidationErrors, tr.Error)
		}
	}
	r.finalizeCollections()
	switch {
	case len(r.ValidationErrors) > 0:
		// Input validation failed before delivery could be trusted.
		r.ExitCode = 3
	case len(r.FailedTracks) > 0:
		r.ExitCode = 4
	case len(r.Unrecoverable) > 0:
		// Processing completed but some media can never be delivered.
		r.ExitCode = 4
	default:
		r.ExitCode = 0
	}
}

// percentile returns the p-quantile of an already-sorted slice.
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if p <= 0 {
		return sorted[0]
	}
	if p >= 1 {
		return sorted[len(sorted)-1]
	}
	idx := int(float64(len(sorted)) * p)
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// Healthy reports whether the run met the acceptance targets: sync within
// 20 ms, recovery at least 90% and P95 latency under 500 ms.
func (r *Report) Healthy() bool {
	return r.SyncErrorMS <= 20 && r.RecoveryRate >= 0.90 && r.LatencyP95MS <= 500
}

// Catalog summarizes which media kinds and codecs were validated, useful for
// the human-readable CLI output.
func (r *Report) Catalog() (video int, audioTracks int, formats []string) {
	seen := make(map[string]bool)
	for _, tr := range r.Tracks {
		switch tr.Kind {
		case timeline.KindVideo:
			video++
			if tr.Index != nil {
				f := string(tr.Index.Format)
				if !seen[f] {
					seen[f] = true
					formats = append(formats, f)
				}
			}
		case timeline.KindAudio:
			audioTracks++
		}
	}
	sort.Strings(formats)
	return video, audioTracks, formats
}

// FailedCode returns the first failed track's error code, or "".
func (r *Report) FailedCode() string {
	for _, tr := range r.Tracks {
		if tr.Status == StatusFailed && tr.Error != nil {
			return tr.Error.Code
		}
	}
	return ""
}

// Adjustments flattens non-fatal notices for terminal display.
func (r *Report) Adjustments() []*core.Error {
	var out []*core.Error
	for _, tr := range r.Tracks {
		out = append(out, tr.Adjustments...)
	}
	return out
}
