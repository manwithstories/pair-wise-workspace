package main

import (
	"errors"
	"fmt"
	"io"

	"mediasync/internal/model"
	"mediasync/internal/pipeline"
)

func fail(err error, rep *pipeline.Report, stdout io.Writer) int {
	code := model.CodeOf(err)
	fmt.Fprintf(stdout, "error_code=%s\n", code)
	var ce *model.CodedError
	if errors.As(err, &ce) {
		if ce.Offset != 0 {
			fmt.Fprintf(stdout, "offset=%d\n", ce.Offset)
		}
		if ce.Track != 0 {
			fmt.Fprintf(stdout, "track_id=%d\n", ce.Track)
		}
		if ce.Message != "" {
			fmt.Fprintf(stdout, "error_message=%s\n", sanitize(ce.Message))
		}
	} else {
		fmt.Fprintf(stdout, "error_message=%s\n", sanitize(err.Error()))
	}
	if rep != nil && rep.CheckpointRecovered {
		fmt.Fprintf(stdout, "checkpoint_recovered=%d\n", rep.RecoveredBlocks)
	}
	return model.ExitCode(err)
}

// printReport emits the agreed key=value lines, in a stable order.
func printReport(w io.Writer, rep *pipeline.Report, resumed bool) {
	fmt.Fprintf(w, "track_count=%d\n", rep.TrackCount)
	fmt.Fprintf(w, "sample_count=%d\n", rep.SampleCount)
	fmt.Fprintf(w, "target_kbps=%d\n", rep.TargetKbps)
	fmt.Fprintf(w, "actual_kbps=%.1f\n", rep.ActualKbps)
	fmt.Fprintf(w, "bitrate_error_pct=%.2f\n", rep.BitrateErrPct)
	fmt.Fprintf(w, "sample_sha256_match=%t\n", rep.SampleSHAMatch)
	fmt.Fprintf(w, "quality_mean=%.6f\n", rep.QualityMean)
	fmt.Fprintf(w, "quality_p95=%.6f\n", rep.QualityP95)
	fmt.Fprintf(w, "manifest_alignment_ms=%.6f\n", rep.ManifestAlignMs)
	fmt.Fprintf(w, "checkpoint_blocks=%d\n", rep.CheckpointBlocks)
	if resumed {
		fmt.Fprintf(w, "resumed_blocks=%d\n", rep.RecoveredBlocks)
	}
	fmt.Fprintf(w, "rtp_received=%d\n", rep.RTPReceived)
	fmt.Fprintf(w, "rtp_duplicates=%d\n", rep.RTPDuplicates)
	fmt.Fprintf(w, "rtp_out_of_order=%d\n", rep.RTPOutOfOrder)
	fmt.Fprintf(w, "incomplete_intervals=%d\n", len(rep.Incomplete))
	if rep.CheckpointRecovered {
		fmt.Fprintf(w, "checkpoint_recovered=%d\n", rep.RecoveredBlocks)
	}
	for _, t := range rep.Tracks {
		fmt.Fprintf(w, "track=name:%s id:%d kind:%s samples:%d bytes:%d sha256:%s\n",
			t.Name, t.TrackID, t.Kind, t.Samples, t.Bytes, t.SHA256)
	}
	if rep.BitrateNote != "" {
		fmt.Fprintf(w, "bitrate_note=%s\n", sanitize(rep.BitrateNote))
	}
	fmt.Fprintf(w, "final_hash=%s\n", rep.FinalHash)
	for _, warning := range rep.Warnings {
		fmt.Fprintf(w, "warning=%s\n", sanitize(warning))
	}
}
