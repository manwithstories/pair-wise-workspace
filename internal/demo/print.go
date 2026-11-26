package demo

import (
	"fmt"
	"io"
	"sort"

	"avpipe/internal/media"
	"avpipe/internal/pipeline"
)

// PrintReport writes the full demo report, including per-segment metrics and
// every required aggregate field, to w.
func PrintReport(w io.Writer, rep *pipeline.Report, opt Options) {
	fmt.Fprintln(w, "== avpipe demo report ==")
	fmt.Fprintf(w, "frames=%d fps=%d segment=%s targets=%v seed=%d faults=%s\n\n",
		opt.Frames, opt.FPS, opt.Segment, opt.Targets, opt.Seed, opt.Faults)

	fmt.Fprintln(w, "-- per-segment representation metrics --")
	fmt.Fprintf(w, "%-5s %-10s %-11s %-11s %-10s %-10s %-10s %-8s\n",
		"group", "variant", "target_kbps", "actual_kbps", "delta_pct",
		"psnr_y_db", "align_ms", "av_drift")
	rows := append([]pipeline.SegmentReport(nil), rep.Segments...)
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Group != rows[j].Group {
			return rows[i].Group < rows[j].Group
		}
		return rows[i].TargetKbps < rows[j].TargetKbps
	})
	for _, s := range rows {
		fmt.Fprintf(w, "%-5d %-10s %-11d %-11.1f %-10.2f %-10.2f %-10.3f %-8.2f\n",
			s.Group, "v"+itoa(s.TargetKbps)+"k", s.TargetKbps,
			float64(s.ActualBps)/1000, s.DeltaPct, s.PSNR,
			s.DurationMS-1000.0, s.DriftMS)
	}

	fmt.Fprintln(w, "\n-- transport recovery --")
	fmt.Fprintf(w, "exact_duplicates=%d conflicting_duplicates=%d reordered=%d wraparounds=%d\n",
		rep.Dedup.ExactDuplicates, rep.Dedup.ConflictingDuplicates,
		rep.Dedup.Reordered, rep.Dedup.Wraparounds)
	fmt.Fprintf(w, "recovered_packets=%d gap_frames=%d burst_loss_blocks=%d corrupt_packets=%d\n",
		rep.RecoveredPackets, rep.GapFrames, rep.FEC.BurstLossBlocks, rep.CorruptPackets)

	fmt.Fprintln(w, "\n-- aggregate acceptance metrics --")
	fmt.Fprintf(w, "target_kbps=%v\n", opt.Targets)
	actual := make([]float64, 0, len(opt.Targets))
	delta := make([]float64, 0, len(opt.Targets))
	psnr := make([]float64, 0, len(opt.Targets))
	for _, t := range opt.Targets {
		var bpsSum int64
		var dSum, pSum float64
		n := 0
		for _, s := range rep.Segments {
			if s.TargetKbps == t {
				bpsSum += s.ActualBps
				dSum += s.DeltaPct
				pSum += s.PSNR
				n++
			}
		}
		if n > 0 {
			actual = append(actual, float64(bpsSum)/1000/float64(n))
			delta = append(delta, dSum/float64(n))
			psnr = append(psnr, pSum/float64(n))
		}
	}
	fmt.Fprintf(w, "actual_kbps=%s\n", joinFloats(actual, 1))
	fmt.Fprintf(w, "delta_pct=%s\n", joinFloats(delta, 2))
	fmt.Fprintf(w, "psnr_y_db=%s\n", joinFloats(psnr, 2))
	fmt.Fprintf(w, "recovered_packets=%d gap_frames=%d\n", rep.RecoveredPackets, rep.GapFrames)
	fmt.Fprintf(w, "align_max_ms=%.3f av_drift_ms=%.2f resume_segments=%d\n",
		rep.MaxAlignMS(), rep.MaxDriftMS(), rep.ResumeSegments)
	fmt.Fprintf(w, "playlist_hash=%s\n", pipeline.HashHex(rep.PlaylistHash))
	fmt.Fprintf(w, "frame_hash=%s\n", pipeline.HashHex(rep.FrameHash))

	if len(rep.Errors) > 0 {
		fmt.Fprintln(w, "\n-- error categories observed --")
		seen := map[media.ErrorClass]int{}
		var order []media.ErrorClass
		for _, e := range rep.Errors {
			if _, ok := seen[e.Class]; !ok {
				order = append(order, e.Class)
			}
			seen[e.Class]++
		}
		for _, c := range order {
			fmt.Fprintf(w, "%s=%d\n", c, seen[c])
		}
	}
}
