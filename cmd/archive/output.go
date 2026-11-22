package main

import (
	"fmt"
	"strings"

	"example.com/medialab/archive/internal/pipeline"
)

// formatReport prints the parseable demo fields. Values are computed from the
// run; nothing here is a fixed success constant.
func formatReport(rep pipeline.Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "AUDIO_VIDEO_SYNC_P95_MS=%d\n", rep.SyncP95)
	fmt.Fprintf(&b, "PACKET_LOSS_RECOVERY_RATE=%.2f\n", rep.RecoveryRate)
	fmt.Fprintf(&b, "PIPELINE_P95_MS=%d\n", rep.PipelineP95)
	fmt.Fprintf(&b, "REPRESENTATION_BOUNDARY_MAX_DELTA_MS=%d\n", rep.BoundaryMaxDelta/1000)
	fmt.Fprintf(&b, "RESUME_OUTPUT_SHA256_MATCH=%t\n", rep.OutputSHA256Match)
	fmt.Fprintf(&b, "REPLAY_VERSION_COUNT=%d\n", rep.ReplayVersionCount)
	// Secondary, human-oriented detail.
	fmt.Fprintf(&b, "OUTPUT_SHA256=%s\n", rep.OutputSHA256)
	fmt.Fprintf(&b, "QUARANTINED_PACKETS=%d\n", rep.Quarantined)
	fmt.Fprintf(&b, "DUPLICATE_PACKETS=%d\n", rep.Duplicates)
	fmt.Fprintf(&b, "REPLAYED_SEGMENTS=%d\n", rep.ReplayedSegments)
	fmt.Fprintf(&b, "SEGMENTS_COMMITTED=%d\n", rep.Segments)
	fmt.Fprintf(&b, "MANIFEST_ENTRIES=%d\n", rep.ManifestEntries)
	fmt.Fprintf(&b, "LEDGER_ENTRIES=%d\n", rep.LedgerEntries)
	fmt.Fprintf(&b, "LOSS_HOLES=%d\n", rep.LossHoles)
	fmt.Fprintf(&b, "RECOVERED_LOSS_HOLES=%d\n", rep.RecoveredLoss)
	fmt.Fprintf(&b, "CORRUPT_HOLES=%d\n", rep.CorruptHoles)
	fmt.Fprintf(&b, "UNRECOVERABLE_GAPS=%d\n", rep.Unrecoverable)
	fmt.Fprintf(&b, "ESTIMATED_SILENCE_US=%d\n", rep.EstimatedUS)
	fmt.Fprintf(&b, "ENCODED_BYTES=%d\n", rep.EncodedBytes)
	fmt.Fprintf(&b, "BOUNDARY_GRID_DELTA_MS=%d\n", rep.BoundaryGridDelta/1000)
	fmt.Fprintf(&b, "BOUNDARY_MAX_SKEW_MS=%d\n", rep.BoundaryMaxSkew/1000)
	fmt.Fprintf(&b, "REPRESENTATIONS=%d\n", rep.Reps)
	fmt.Fprintf(&b, "RESUMED=%t\n", rep.Resumed)
	fmt.Fprintf(&b, "ACCEPTANCE=pass\n")
	if !rep.Verify.Passed() {
		b.Reset()
		fmt.Fprintf(&b, "AUDIO_VIDEO_SYNC_P95_MS=%d\n", rep.SyncP95)
		fmt.Fprintf(&b, "PACKET_LOSS_RECOVERY_RATE=%.2f\n", rep.RecoveryRate)
		fmt.Fprintf(&b, "PIPELINE_P95_MS=%d\n", rep.PipelineP95)
		fmt.Fprintf(&b, "REPRESENTATION_BOUNDARY_MAX_DELTA_MS=%d\n", rep.BoundaryMaxDelta/1000)
		fmt.Fprintf(&b, "RESUME_OUTPUT_SHA256_MATCH=%t\n", rep.OutputSHA256Match)
		fmt.Fprintf(&b, "REPLAY_VERSION_COUNT=%d\n", rep.ReplayVersionCount)
		fmt.Fprintf(&b, "OUTPUT_SHA256=%s\n", rep.OutputSHA256)
		fmt.Fprintf(&b, "ACCEPTANCE=fail sync=%t recovery=%t pipeline=%t boundary=%t resume=%t replay=%t quarantine=%t\n",
			rep.Verify.SyncOK, rep.Verify.RecoveryOK, rep.Verify.PipelineOK, rep.Verify.BoundaryOK,
			rep.Verify.ResumeOK, rep.Verify.ReplayOK, rep.Verify.QuarantineOK)
	}
	return b.String()
}
