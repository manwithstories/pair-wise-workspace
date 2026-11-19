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

	// Sync error is the worst offset between tracks relative to their declared
	// origin. A single video baseline gives zero; a shifted camera gives its
	// declared offset.
	if len(syncOffsets) > 1 {
		var worst timeline.Micros
		for i := 1; i < len(syncOffsets); i++ {
			d := (syncOffsets[i] - syncOffsets[0]).Abs()
			if d > worst {
				worst = d
			}
		}
		r.SyncErrorMS = float64(worst) / 1000
	}
	sort.Float64s(latencies)
	r.LatencyP95MS = percentile(latencies, 0.95)
	r.RecoveryRate = engine.RecoveryRate()
}

// finish decides the exit code and a final consistency pass.
func (r *Report) finish() {
	sort.SliceStable(r.Tracks, func(i, j int) bool { return r.Tracks[i].Track < r.Tracks[j].Track })
	switch {
	case len(r.ValidationErrors) > 0:
		r.ExitCode = 3
	case len(r.FailedTracks) > 0:
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
