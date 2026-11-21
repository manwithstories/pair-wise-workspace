package main

import (
	"fmt"
	"io"
	"strings"

	"mediad/internal/pipeline"
)

func printReport(w io.Writer, res pipeline.Result) {
	s := res.Summary
	fmt.Fprintln(w, "mediad demo report")
	fmt.Fprintln(w, strings.Repeat("-", 46))
	fmt.Fprintf(w, "%-28s %s\n", "seed", s.Seed)
	if len(res.ReusedStages) > 0 {
		fmt.Fprintf(w, "%-28s %s\n", "reused_stages", strings.Join(res.ReusedStages, ","))
	}
	fmt.Fprintf(w, "%-28s %s\n", "rollback_stage", orNone(s.RollbackStage))
	fmt.Fprintf(w, "%-28s %s\n", "resume_stage", orNone(s.ResumeStage))
	if s.InjectedFaults > 0 {
		fmt.Fprintf(w, "%-28s %d\n", "injected_faults", s.InjectedFaults)
	}
	fmt.Fprintln(w, strings.Repeat("-", 46))
	fmt.Fprintf(w, "%-28s %.3f\n", "target_bitrate_kbps", s.TargetBitrateKbps)
	fmt.Fprintf(w, "%-28s %.3f\n", "actual_bitrate_kbps", s.ActualBitrateKbps)
	fmt.Fprintf(w, "%-28s %+.3f\n", "bitrate_delta_pct", s.BitrateDeltaPct)
	fmt.Fprintf(w, "%-28s %.2f\n", "quality_psnr_db", s.QualityPSNRdB)
	fmt.Fprintf(w, "%-28s %d\n", "recovered_packets", s.RecoveredPackets)
	fmt.Fprintf(w, "%-28s %d\n", "unrecoverable_packets", s.UnrecoverablePackets)
	fmt.Fprintf(w, "%-28s %d\n", "duplicate_packets", s.Duplicates)
	fmt.Fprintf(w, "%-28s %d\n", "reordered_packets", s.Reordered)
	fmt.Fprintf(w, "%-28s %.3f\n", "manifest_alignment_ms", s.ManifestAlignmentMS)
	fmt.Fprintf(w, "%-28s %d\n", "index_seek_mismatches", s.IndexSeekMismatches)
	fmt.Fprintf(w, "%-28s %s\n", "manifest_sha256", s.ManifestSHA256)
	if s.RepairWraps+s.RepairDuplicates+s.RepairAdjusted > 0 || s.TimestampDriftPPM > 0 {
		fmt.Fprintf(w, "%-28s wraps=%d duplicates=%d adjusted=%d drift=%.1fppm\n", "timestamp_repairs",
			s.RepairWraps, s.RepairDuplicates, s.RepairAdjusted, s.TimestampDriftPPM)
	}
	if len(s.TrackBitrates) > 0 {
		fmt.Fprintln(w, strings.Repeat("-", 46))
		fmt.Fprintf(w, "%-6s %12s %12s %9s\n", "track", "target_kbps", "actual_kbps", "delta_pct")
		for _, t := range s.TrackBitrates {
			fmt.Fprintf(w, "%-6d %12.2f %12.2f %+9.2f\n", t.TrackID, t.TargetBitrateKbps, t.ActualBitrateKbps, t.DeltaPct)
		}
	}
	if len(res.Notes) > 0 {
		fmt.Fprintln(w, strings.Repeat("-", 46))
		for _, n := range res.Notes {
			fmt.Fprintf(w, "note: %s\n", n)
		}
	}
}
