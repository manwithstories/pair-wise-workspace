package main

import (
	"fmt"
	"io"
	"sort"

	"archivearchivist/internal/model"
	"archivearchivist/internal/mp4"
	"archivearchivist/internal/pipeline"
	"archivearchivist/internal/report"
)

func printAnalysis(w io.Writer, a *pipeline.Analysis, cmd string) {
	fmt.Fprintf(w, "archivist %s\n", cmd)
	fmt.Fprintf(w, "tracks=%d scans=%d\n", a.Metrics.Tracks, len(a.Scans))
	for _, sc := range a.Scans {
		fmt.Fprintf(w, "  %s brand=%s bytes=%d tracks=%d\n",
			baseName(sc.Path), sc.Brand, sc.Bytes, len(sc.Tracks))
		for _, tr := range sc.Tracks {
			fmt.Fprintf(w, "    track=%d kind=%s codec=%s timescale=%d samples=%d keyframes=%d vfr=%v\n",
				tr.ID, tr.Kind, tr.Codec, tr.Timescale, len(tr.Samples), countKeyframes(tr), !tr.ConstantFrame)
			if len(tr.KeyframeGaps) > 0 {
				fmt.Fprintf(w, "    keyframe_gap_ticks p50=%d p95=%d\n",
					tr.KeyframeGapP50, tr.KeyframeGapP95)
			}
		}
		if len(sc.CorruptRanges) > 0 {
			for _, r := range sc.CorruptRanges {
				fmt.Fprintf(w, "    corrupt bytes=[%d,%d]\n", r.Start, r.End)
			}
		}
	}
	if a.Capture != nil {
		fmt.Fprintf(w, "capture received=%d accepted=%d duplicates=%d reordered=%d late=%d gap=%d recovery=%s\n",
			a.Capture.Received, a.Capture.Accepted, a.Capture.Duplicates, a.Capture.OutOfOrder,
			a.Capture.Late, a.Capture.Missing, report.FormatRate(a.Capture.RecoveryRate()))
	}
	for _, id := range sortedClockKeys(a) {
		cv := a.Clocks[id]
		fmt.Fprintf(w, "clock track=%s wraps=%d drift_ppm=%s review=%v max_skew_us=%d\n",
			cv.Track, cv.Wraps, cv.DriftPPM, cv.DriftReview, cv.MaxSkewUS)
	}
	for _, r := range a.Plan.Ranges {
		fmt.Fprintf(w, "range track=%s bytes=[%d,%d] before_keyframe=%d after_keyframe=%d recoverable=[%d,%d] error_bound_us=%d status=%s\n",
			r.Track, r.CorruptStart, r.CorruptEnd, r.BeforeKeyframe, r.AfterKeyframe,
			r.RecoverableStart, r.RecoverableEnd, r.ErrorBoundUS, r.Status)
	}
	for _, b := range a.Plan.Boundaries {
		fmt.Fprintf(w, "boundary clip=%s track=%s edge=%s intended=%s keyframe=%d snap_us=%d status=%s\n",
			b.Clip, b.Track, b.Edge, b.Intended, b.Keyframe, b.ErrorUS, b.Status)
	}
	for _, tr := range a.Plan.Transitions {
		fmt.Fprintf(w, "crossfade id=%s from=%s to=%s skew_us=%d status=%s\n",
			tr.ID, tr.From, tr.To, tr.SkewUS, tr.Status)
	}
	printMetrics(w, "summary", a.Metrics)
	printFaults(w, a.Faults)
}

func printBench(w io.Writer, res *pipeline.BenchResult) {
	a := res.Analysis
	fmt.Fprintf(w, "archivist bench iterations=%d\n", res.Iterations)
	fmt.Fprintf(w, "latency_us mean=%d p95=%d max=%d\n", res.LatencyMeanUS, res.LatencyP95US, res.LatencyMaxUS)
	fmt.Fprintf(w, "sync_error_us p95=%d max=%d samples=%d\n", res.SyncP95US, res.SyncMaxUS, a.Metrics.SyncSamples)
	fmt.Fprintf(w, "packet_recovery_rate=%s recovered=%d gap=%d\n",
		report.FormatRate(res.RecoveryRate), a.Metrics.RecoveredPackets, a.Metrics.TotalGap)
	printMetrics(w, "summary", a.Metrics)
	printFaults(w, a.Faults)
}

func printMetrics(w io.Writer, label string, m report.Metrics) {
	fmt.Fprintf(w, "%s tracks=%d corrupt_ranges=%d recovered_ranges=%d unrecoverable_ranges=%d\n",
		label, m.Tracks, m.CorruptRanges, m.RecoveredRanges, m.UnrecoverableRanges)
	fmt.Fprintf(w, "acceptance tracks=%d corrupt_ranges>=1:%v wraps=%d wraps>=1:%v constant_fps=%v\n",
		m.Tracks, m.CorruptRanges >= 1, m.Wraps, m.Wraps >= 1, m.ConstantFPS)
	fmt.Fprintf(w, "acceptance sync_error_p95_ms=%s sync_error_p95_ms<=20:%v\n",
		report.FormatMillis(m.SyncErrorP95US), m.SyncErrorP95US <= 20_000)
	fmt.Fprintf(w, "acceptance packet_recovery_rate=%s packet_recovery_rate>=0.90:%v\n",
		report.FormatRate(m.PacketRecoveryRate), m.PacketRecoveryRate.Cmp(model.NewRat(9, 10)) >= 0)
	fmt.Fprintf(w, "acceptance latency_p95_ms=%s latency_p95_ms<=250:%v (iterations=%d)\n",
		report.FormatMillis(m.LatencyP95US), m.LatencyP95US <= 250_000, m.Iterations)
	fmt.Fprintf(w, "metrics keyframe_snap_p95_ms=%s crossfade_skew_p95_ms=%s boundaries recovered=%d blocked=%d\n",
		report.FormatMillis(m.SnapErrorP95US), report.FormatMillis(m.CrossfadeSkewP95US),
		m.RecoveredBoundaries, m.BlockedBoundaries)
	if len(m.DriftReviewTracks) > 0 {
		fmt.Fprintf(w, "drift_review_tracks=%v\n", m.DriftReviewTracks)
	}
}

func printFaults(w io.Writer, fs model.FaultList) {
	fmt.Fprintf(w, "faults total=%d\n", len(fs))
	limit := 40
	for i, f := range fs {
		if i >= limit {
			fmt.Fprintf(w, "  ... %d more\n", len(fs)-limit)
			break
		}
		fmt.Fprintf(w, "  %s\n", f)
	}
}

func countKeyframes(tr mp4.Track) int {
	n := 0
	for _, s := range tr.Samples {
		if s.Keyframe && !s.Corrupt {
			n++
		}
	}
	return n
}

func sortedClockKeys(a *pipeline.Analysis) []string {
	out := make([]string, 0, len(a.Clocks))
	for k := range a.Clocks {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func baseName(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' || p[i] == '\\' {
			return p[i+1:]
		}
	}
	return p
}
