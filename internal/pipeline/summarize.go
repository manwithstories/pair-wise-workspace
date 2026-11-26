package pipeline

import (
	"sort"
	"time"

	"audiorestore/internal/model"
	"audiorestore/internal/report"
	"audiorestore/internal/timeline"
)

// summarize aggregates per-track outputs into the run summary and
// verifies the standing invariants: PTS monotonicity per track and full
// segment provenance. Sync P95 is the cross-rate mapping residual of each
// video anchor on every track clock.
func (r *Run) summarize(g *timeline.Graph, results []*TrackResult) *report.Summary {
	s := &report.Summary{TrackCount: len(results), Degraded: r.log.Degraded()}

	var blockTimes, stageTimes []time.Duration
	var syncUS []int64
	totalRecovered, totalUnrecov := 0, 0

	for _, res := range results {
		blockTimes = append(blockTimes, res.BlockTimes...)
		stageTimes = append(stageTimes, res.StageTimes...)
		totalRecovered += len(res.Recovered)
		totalUnrecov += len(res.Unrecov)
		s.RecoveredBlocks = append(s.RecoveredBlocks, res.Recovered...)
		s.UnrecoveredBlocks = append(s.UnrecoveredBlocks, res.Unrecov...)
		s.Segments += len(res.Segments)
		s.SilenceFrames += silenceCount(res)
		for _, seg := range res.Segments {
			if len(seg.SourceBlocks) == 0 {
				r.log.Addf(model.ErrInvalidParam, seg.Track, -1,
					seg.StartFrame, "segment %s has no source block", seg.ID)
			}
		}
		if err := checkTrackMonotonic(res); err != nil {
			if te, ok := err.(model.TrackedError); ok {
				r.log.Add(te)
			}
		}
		for _, a := range r.anchors {
			// Sync P95 measures cross-rate integer mapping residuals: each
			// video anchor's exact PTS versus its nearest integer frame on
			// every track clock (bounded by half a sample period).
			us, err := timeline.MappingResidualUS(res.Track, a)
			if err == nil {
				syncUS = append(syncUS, us)
			}
		}
	}

	s.AVSyncP95US = percentileInt64(syncUS, 95)
	s.LossRecoveryPct = report.RecoveryPercent(totalRecovered, totalUnrecov)
	s.DedupHits = r.table.Hits()
	s.StorageIDs = r.table.StorageIDs()
	s.FingerprintHashes = r.table.SortedHashes()
	s.Errors = r.log.All()

	// Pipeline P95 is the slower of per-block and per-stage nearest-rank
	// P95, expressed in milliseconds.
	p95 := report.Percentile(blockTimes, 95)
	if d := report.Percentile(stageTimes, 95); d > p95 {
		p95 = d
	}
	s.PipelineP95MS = float64(p95.Microseconds()) / 1000.0
	return s
}

// checkTrackMonotonic verifies PTS ordering on one track's own clock.
func checkTrackMonotonic(res *TrackResult) error {
	var pts []timeline.TrackPTS
	for _, seg := range res.Segments {
		pts = append(pts, timeline.TrackPTS{
			Track: res.Track.ID, Frame: seg.StartFrame, PTS: seg.StartPTS,
		})
	}
	return timeline.CheckMonotonic(pts)
}

func silenceCount(res *TrackResult) int64 {
	var n int64
	for _, rng := range res.Silence {
		n += rng.EndFrame - rng.StartFrame
	}
	return n
}

// percentileInt64 is the nearest-rank P-th percentile on a sorted copy.
func percentileInt64(values []int64, p int) int64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]int64(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	rank := (p*len(sorted) + 99) / 100
	return sorted[rank-1]
}
