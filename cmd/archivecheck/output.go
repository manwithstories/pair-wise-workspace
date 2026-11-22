package main

import (
	"fmt"
	"os"

	"archivecheck/internal/fault"
	"archivecheck/internal/pipeline"
)

// printReport renders one path's metrics in the acceptance key=value form.
func printReport(name string, r *pipeline.Report) {
	s := r.Stats
	fmt.Printf("=== %s ===\n", name)
	fmt.Printf("target_bps=%d\n", s.Bitrate.TargetBps)
	fmt.Printf("actual_bps=%d\n", s.Bitrate.ActualBps)
	fmt.Printf("bitrate_delta_pct=%.2f\n", s.Bitrate.DeltaPct)
	fmt.Printf("quality_psnr_db=%.2f\n", s.Quality.PSNRdB)
	fmt.Printf("quality_samples=%d\n", s.Quality.Samples)
	fmt.Printf("manifest_alignment_ms=%.3f\n", s.ManifestAlignMS)
	fmt.Printf("lost_packets=%d\n", s.LostPackets)
	fmt.Printf("duplicate_packets=%d\n", s.DuplicatePackets)
	fmt.Printf("reordered_packets=%d\n", s.ReorderedPackets)
	fmt.Printf("timestamp_repairs=%d\n", s.TimestampRepairs)
	fmt.Printf("index_entries=%d\n", s.IndexEntries)
	fmt.Printf("rollback_count=%d\n", s.RollbackCount)
	fmt.Printf("index_sha256=%s\n", s.IndexSHA256)
	fmt.Printf("blocks=%d\n", r.Blocks)
	fmt.Printf("spilled_chunks=%d\n", r.Spill.Spilled)
	fmt.Printf("peak_memory_bytes=%d\n", r.Spill.PeakMemory)
	fmt.Printf("source_truncated=%v\n", r.Truncated)
	fmt.Printf("committed_stages=%v\n", r.Stages)
	fmt.Printf("resumed_stages=%v\n", r.Resumed)
	if len(r.Faults) > 0 {
		fmt.Println("faults:")
		for _, f := range r.Faults {
			fmt.Printf("  - %s\n", f.Error())
		}
	}
	fmt.Printf("exit_code=%d\n\n", r.ExitCode)
}

// printDeterminism confirms the resumed run reproduces the normal digest.
func printDeterminism(normal, resumed *pipeline.Report) {
	same := normal.Stats.IndexSHA256 == resumed.Stats.IndexSHA256
	fmt.Printf("index_digest_stable=%v\n", same)
	fmt.Printf("determinism_signature=%s\n", normal.Stats.Signature())
	if !same {
		fmt.Fprintf(os.Stderr, "archivecheck: resumed index digest differs from normal run\n")
	}
	if normal.ExitCode == fault.ExitOK && resumed.ExitCode != fault.ExitOK {
		fmt.Fprintf(os.Stderr, "archivecheck: resumed run was not clean\n")
	}
}
