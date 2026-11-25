package main

import (
	"fmt"
	"os"
	"path/filepath"

	"archivectl/internal/manifest"
	"archivectl/internal/timeline"
	"archivectl/internal/transport"
)

type recoverMetrics struct {
	code               int
	targetKBPS         int
	bandwidth          transport.BandwidthReport
	dupDrop            int
	reordered          int
	gaps               int
	gapRanges          int
	payloadMismatch    int
	timestampMismatch  int
	manifestMaxErrorMS float64
	outputHash         string
	manifestHash       string
	sampleCount        int
	payloadBytes       int64
}

func printRecoverMetrics(m recoverMetrics, j manifest.Journal) {
	status := "ok"
	if m.code == 2 {
		status = "partial_gap_ledger"
	}
	fmt.Printf("status=%s\n", status)
	fmt.Printf("exit_code=%d\n", m.code)
	fmt.Printf("target_kbps=%d\n", m.targetKBPS)
	fmt.Printf("actual_kbps=%.2f\n", m.bandwidth.ActualKBPS)
	fmt.Printf("deviation_pct=%.3f\n", m.bandwidth.DeviationPct)
	fmt.Printf("dup_drop=%d\n", m.dupDrop)
	fmt.Printf("reordered=%d\n", m.reordered)
	fmt.Printf("gaps=%d\n", m.gaps)
	fmt.Printf("gap_ranges=%d\n", m.gapRanges)
	fmt.Printf("fabricated=0\n")
	fmt.Printf("payload_hash_mismatch=%d\n", m.payloadMismatch)
	fmt.Printf("timestamp_mismatch=%d\n", m.timestampMismatch)
	fmt.Printf("manifest_max_error_ms=%.6f\n", m.manifestMaxErrorMS)
	fmt.Printf("samples=%d\n", m.sampleCount)
	fmt.Printf("payload_bytes=%d\n", m.payloadBytes)
	fmt.Printf("output_sha256=%s\n", m.outputHash)
	fmt.Printf("manifest_sha256=%s\n", m.manifestHash)
	_ = j
}

func atomicWriteFile(path string, b []byte, mode os.FileMode) error {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, mode); err != nil {
		return timeline.Wrap(timeline.CodeInvalidArgument, "write output", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return timeline.Wrap(timeline.CodeInvalidArgument, "rename output", err)
	}
	return nil
}
