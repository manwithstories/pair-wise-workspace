package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"mkvarchive/internal/pipeline"
	"mkvarchive/internal/remux"
)

func byteReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }

// printDemoFields emits the exact key=value shape the acceptance checks grep.
func printDemoFields(res *pipeline.Result) {
	q := res.Quality
	fmt.Printf("outcome=%s exit_code=%d\n", res.Outcome, res.Outcome.ExitCode())
	fmt.Printf("target_bitrate_kbps=%.0f actual_bitrate_kbps=%.1f "+
		"bitrate_deviation_pct=%.2f\n",
		q.TargetBitrateKbps, q.ActualBitrateKbps, q.BitrateDeviationPct)
	fmt.Printf("payload_sha256_match=%t quality_blocks=%d "+
		"manifest_alignment_ms=%.3f\n",
		q.PayloadSHA256Match, q.QualityBlocks, q.ManifestAlignmentMS)
	if len(res.Warnings) > 0 {
		for _, w := range res.Warnings {
			fmt.Printf("warning code=%s offset=%d %s\n", w.Code, w.Offset, w.Message)
		}
	}
}

func printRecoverFields(res *pipeline.Result) {
	fmt.Printf("recovered_blocks=%d total_source_blocks=%d\n",
		res.RecoveredBlocks, res.TotalSourceBlocks)
	fmt.Printf("first_error_offset=%d first_error_code=%s\n",
		res.FirstErrorOffset, res.FirstErrorCode)
	fmt.Printf("exit_code=%d\n", res.Outcome.ExitCode())
}

func printVerifyFields(vr *pipeline.VerifyResult) {
	fmt.Printf("target_bitrate_kbps=%.0f actual_bitrate_kbps=%.1f "+
		"bitrate_deviation_pct=%.2f\n",
		vr.TargetBitrateKbps, vr.ActualBitrateKbps, vr.BitrateDeviationPct)
	fmt.Printf("payload_sha256_match=%t quality_blocks=%d "+
		"manifest_alignment_ms=%.3f\n",
		vr.PayloadSHA256Match, vr.QualityBlocks, vr.ManifestAlignmentMS)
}

// writeQualityEvidence stores the QA report as JSON and additionally frames
// it inside an MP4 sidecar via go-mp4, proving the box round trip.
func writeQualityEvidence(dir string, res *pipeline.Result) error {
	raw, err := json.MarshalIndent(res.Quality, "", "  ")
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(dir, "quality.json"), raw, 0o644); err != nil {
		return err
	}
	sf, err := os.Create(filepath.Join(dir, "evidence.mp4"))
	if err != nil {
		return err
	}
	defer sf.Close()
	return remux.WriteQualitySidecar(sf, raw)
}
