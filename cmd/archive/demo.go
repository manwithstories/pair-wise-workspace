package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"archive/internal/batch"
	"archive/internal/pipeline"
)

// report is the stable, serializable verification report.
type report struct {
	RoundtripRate    float64  `json:"roundtrip_rate"`
	KeyframeHitRate  float64  `json:"keyframe_hit_rate"`
	Segments         int      `json:"segments"`
	SegmentSHA256    []string `json:"segment_sha256"`
	LostPackets      int64    `json:"lost_packets"`
	DuplicatePackets int      `json:"duplicate_packets"`
	PeakBuffer       int      `json:"peak_buffer_packets"`
	RecoveredChunks  int      `json:"recovered_chunks"`
	Frames           int64    `json:"frames"`
	Truncated        int      `json:"truncated_packets"`
	Canceled         bool     `json:"canceled"`
}

func buildReport(st *pipeline.Stats) report {
	shas := make([]string, len(st.SegmentSHA))
	for i, h := range st.SegmentSHA {
		shas[i] = hex.EncodeToString(h[:])
	}
	return report{
		RoundtripRate: st.RoundtripRate, KeyframeHitRate: st.KeyframeHitRate,
		Segments: st.Segments, SegmentSHA256: shas, LostPackets: st.Lost,
		DuplicatePackets: st.Duplicates, PeakBuffer: st.PeakBuffer,
		RecoveredChunks: st.RecoveredChunks, Frames: st.Frames,
		Truncated: st.Truncated, Canceled: st.Canceled,
	}
}

func runDemo(ctx context.Context, args []string) int {
	cfg, jsonOut, err := parseDemoFlags(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "parameter error:", err)
		return exitParam
	}
	if cfg.OutDir == "" {
		fmt.Fprintln(os.Stderr, "parameter error: --out is required")
		return exitParam
	}
	if err := os.MkdirAll(cfg.OutDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "io error:", err)
		return exitCanceled
	}

	st, err := pipeline.Run(ctx, cfg)
	if st != nil {
		// Persist run state so verify and a later resume can find it.
		_ = batch.Save(cfg.OutDir, batch.State{
			InputDigest: st.InputDigest, ParamDigest: st.ParamDigest,
			CompleteChunks: st.RecoveredChunks, Segments: st.Segments, Frames: st.Frames,
		})
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "run failed:", err)
		return exitFor(err)
	}
	_ = batch.MarkFinished(cfg.OutDir, batch.State{
		InputDigest: st.InputDigest, ParamDigest: st.ParamDigest,
		CompleteChunks: st.RecoveredChunks, Segments: st.Segments, Frames: st.Frames,
	})

	rep := buildReport(st)
	if jsonOut {
		_ = json.NewEncoder(os.Stdout).Encode(rep)
	} else {
		printReport(rep)
	}
	// Persist the report next to the artifacts for offline auditing.
	if err := writeReportFile(cfg.OutDir, rep); err != nil {
		fmt.Fprintln(os.Stderr, "io error:", err)
		return exitCanceled
	}
	return exitOK
}

func printReport(r report) {
	fmt.Printf("roundtrip_rate=%.3f\n", r.RoundtripRate)
	fmt.Printf("keyframe_hit_rate=%.3f\n", r.KeyframeHitRate)
	fmt.Printf("segments=%d\n", r.Segments)
	for i, h := range r.SegmentSHA256 {
		fmt.Printf("segment_sha256[%d]=%s\n", i, h)
	}
	fmt.Printf("lost_packets=%d\n", r.LostPackets)
	fmt.Printf("duplicate_packets=%d\n", r.DuplicatePackets)
	fmt.Printf("peak_buffer_packets=%d\n", r.PeakBuffer)
	fmt.Printf("recovered_chunks=%d\n", r.RecoveredChunks)
}

func writeReportFile(outDir string, r report) error {
	path := filepath.Join(outDir, "report.json")
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
