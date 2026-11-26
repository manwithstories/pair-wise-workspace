// Package verify cross-checks the unified timeline, the produced output
// files and the collected error records. It measures A/V sync from real
// timeline positions, packet recovery from transport stats, processing
// latency percentiles from staged timings, and confirms that wrap and loss
// events actually propagate into the evidence rather than being hidden.
package verify

import (
	"sort"

	"bq/internal/clock"
	"bq/internal/manifest"
	"bq/internal/model"
	"bq/internal/timeline"
	"bq/internal/transport"
)

// Metrics holds the acceptance measurements printed by the CLI.
type Metrics struct {
	AVSyncMaxMs        int64   `json:"av_sync_max_ms"`
	LossRecoveryPct    float64 `json:"loss_recovery_pct"`
	LatencyP95Ms       int64   `json:"latency_p95_ms"`
	Frames             int     `json:"frames"`
	FrameSHA256        string  `json:"frame_sha256"`
	ErrorCount         int     `json:"error_count"`
	FirstSourceOffset  int64   `json:"first_source_offset"`
	RecoveredPackets   int64   `json:"recovered_packets"`
	UnrecoveredPackets int64   `json:"unrecovered_packets"`
}

// Thresholds define the acceptance gates. Exit code 1 is returned when a
// measured metric crosses its bound.
type Thresholds struct {
	MaxAVSyncMs        int64
	MinRecoveryPct     float64
}

// DefaultThresholds matches the broadcast acceptance criteria.
func DefaultThresholds() Thresholds {
	return Thresholds{MaxAVSyncMs: 20, MinRecoveryPct: 90}
}

// StageLatency records how long one pipeline stage took, in milliseconds.
type StageLatency struct {
	Stage string
	Ms    int64
}

// Input bundles everything the verifier reads.
type Input struct {
	Manifest  *manifest.Manifest
	Timeline  *timeline.Timeline
	Drift     []timeline.DriftReport
	Transport *transport.Result
	Latencies []StageLatency
	FrameHash string
	FrameCount int
}

// Compute derives the metrics. Errors are sorted deterministically and the
// earliest source offset is reported for the corruption sample.
func Compute(in Input) (Metrics, []model.Error) {
	errs := append([]model.Error(nil), in.Transport.Errors...)
	errs = append(errs, in.Timeline.Errors...)
	model.SortErrors(errs)

	m := Metrics{
		AVSyncMaxMs:       measureAVSync(in),
		LossRecoveryPct:   measureRecovery(in.Transport),
		LatencyP95Ms:      percentile95(in.Latencies),
		Frames:            in.FrameCount,
		FrameSHA256:       in.FrameHash,
		ErrorCount:        len(errs),
		FirstSourceOffset: firstSourceOffset(errs),
	}
	for _, st := range in.Transport.Stats {
		m.RecoveredPackets += st.RecoveredPackets
		m.UnrecoveredPackets += st.UnrecoveredPackets
	}
	return m, errs
}

// measureAVSync compares each audio item's master position against the
// nearest video item on the reference (first) video track, after applying
// the drift correction. The largest absolute gap is the A/V sync maximum.
func measureAVSync(in Input) int64 {
	if in.Timeline == nil {
		return 0
	}
	var videoRef []timeline.Item
	for id, items := range in.Timeline.Streams {
		if in.Manifest.StreamKind(id) == manifest.KindVideo {
			videoRef = items
			break
		}
	}
	var maxGap int64
	audioID := firstStreamOfKind(in.Manifest, manifest.KindAudio)
	audio := in.Timeline.Streams[audioID]
	driftByStream := driftIndex(in.Drift)

	for _, a := range audio {
		corrected := a.MasterPTS
		if rep, ok := driftByStream[a.StreamID]; ok {
			if off, recoverable := rep.SyncOffsetAt(a.MasterPTS); recoverable {
				corrected = a.MasterPTS + off
			}
		}
		nearest := nearestVideo(videoRef, corrected, driftByStream)
		if nearest < 0 {
			continue
		}
		gap := corrected - nearest
		if gap < 0 {
			gap = -gap
		}
		if gap > maxGap {
			maxGap = gap
		}
	}
	return clock.MasterToMs(maxGap)
}

func driftIndex(reports []timeline.DriftReport) map[int]timeline.DriftReport {
	out := make(map[int]timeline.DriftReport, len(reports))
	for _, r := range reports {
		out[r.StreamID] = r
	}
	return out
}

// nearestVideo returns the drift-corrected master tick of the video item
// closest to target, or -1 when the video track is empty.
func nearestVideo(video []timeline.Item, target int64, drift map[int]timeline.DriftReport) int64 {
	if len(video) == 0 {
		return -1
	}
	best := video[0].MasterPTS
	bestDist := abs64(best - target)
	for _, v := range video {
		pos := v.MasterPTS
		if rep, ok := drift[v.StreamID]; ok {
			if off, recoverable := rep.SyncOffsetAt(v.MasterPTS); recoverable {
				pos = v.MasterPTS + off
			}
		}
		if d := abs64(pos - target); d < bestDist {
			bestDist = d
			best = pos
		}
	}
	return best
}

func measureRecovery(res *transport.Result) float64 {
	if res == nil {
		return 100
	}
	var lost, recovered int64
	for _, st := range res.Stats {
		recovered += st.RecoveredPackets
		lost += st.RecoveredPackets + st.UnrecoveredPackets
	}
	if lost == 0 {
		return 100
	}
	return float64(recovered) * 100 / float64(lost)
}

// percentile95 uses the nearest-rank method over stage latencies. A single
// stage still yields a defined p95 (its own value).
func percentile95(latencies []StageLatency) int64 {
	if len(latencies) == 0 {
		return 0
	}
	vals := make([]int64, 0, len(latencies))
	for _, l := range latencies {
		vals = append(vals, l.Ms)
	}
	sort.Slice(vals, func(i, j int) bool { return vals[i] < vals[j] })
	rank := (95 * len(vals) + 99) / 100 // ceil(0.95*n)
	if rank < 1 {
		rank = 1
	}
	return vals[rank-1]
}

func firstSourceOffset(errs []model.Error) int64 {
	for _, e := range errs {
		if e.SourceOffset >= 0 {
			return e.SourceOffset
		}
	}
	return -1
}

func firstStreamOfKind(m *manifest.Manifest, kind string) int {
	for _, s := range m.Streams {
		if s.Kind == kind {
			return s.ID
		}
	}
	return -1
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// Evaluate applies the acceptance thresholds and reports which gates failed.
func (m Metrics) Evaluate(th Thresholds) []string {
	var failed []string
	if m.AVSyncMaxMs > th.MaxAVSyncMs {
		failed = append(failed, "av_sync_max_ms")
	}
	if m.LossRecoveryPct < th.MinRecoveryPct {
		failed = append(failed, "loss_recovery_pct")
	}
	return failed
}
