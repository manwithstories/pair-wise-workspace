// Command archive restores migrated HLS/DASH archives: it parses
// manifests, dedups and orders segments, recovers RTP transport loss,
// demuxes, remuxes losslessly onto a common timebase with checkpointing,
// and reports verification metrics. Exit codes: 0 = all checks passed,
// 2 = parameter error, 3 = asset failures, 4 = checkpoint/result
// validation failure.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"

	"archive/internal/report"
)

const (
	exitOK           = 0
	exitParamError   = 2
	exitAssetFailed  = 3
	exitVerifyFailed = 4
)

const usage = `archive - restore migrated HLS/DASH media archives

usage:
  archive demo [--seed N] [--burst-loss N] [--reorder N]
               [--bandwidth-mbps 1,8,2] [--chunk-bytes N] [--out DIR] [--json]

exit codes:
  0  all checks passed
  2  parameter error
  3  asset failures after processing
  4  checkpoint or result validation failure
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	os.Exit(run(ctx, os.Args[1:]))
}

func run(ctx context.Context, args []string) int {
	if len(args) == 0 || args[0] != "demo" {
		fmt.Fprint(os.Stderr, usage)
		return exitParamError
	}
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	seed := fs.Int64("seed", 41, "deterministic fixture seed")
	burst := fs.Int("burst-loss", 7, "RTP burst loss length (packets)")
	reorder := fs.Int("reorder", 12, "RTP reorder window (packets)")
	bw := fs.String("bandwidth-mbps", "1,8,2", "comma-separated bandwidth trace in Mbps")
	chunk := fs.Int64("chunk-bytes", 1048576, "checkpoint chunk size in bytes")
	jsonOut := fs.Bool("json", false, "emit the JSON summary")
	outDir := fs.String("out", "", "work/output directory (default: temp dir)")
	if err := fs.Parse(args[1:]); err != nil {
		return exitParamError
	}
	cfg, err := validateDemoConfig(*seed, *burst, *reorder, *bw, *chunk, *jsonOut, *outDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "parameter error:", err)
		return exitParamError
	}
	code, sum := runDemo(ctx, cfg)
	if sum == nil {
		return code
	}
	if cfg.jsonOut {
		if err := report.WriteJSON(os.Stdout, sum); err != nil {
			fmt.Fprintln(os.Stderr, "cannot write report:", err)
			return exitVerifyFailed
		}
	} else {
		printTextSummary(sum)
	}
	return code
}

func validateDemoConfig(seed int64, burst, reorder int, bw string, chunk int64, jsonOut bool, outDir string) (demoConfig, error) {
	cfg := demoConfig{seed: seed, burstLoss: burst, reorder: reorder, chunkBytes: chunk, jsonOut: jsonOut, outDir: outDir}
	if burst < 0 || reorder < 0 {
		return cfg, fmt.Errorf("burst-loss and reorder must be >= 0")
	}
	if chunk <= 0 {
		return cfg, fmt.Errorf("chunk-bytes must be > 0")
	}
	for _, part := range strings.Split(bw, ",") {
		v, err := strconv.ParseFloat(strings.TrimSpace(part), 64)
		if err != nil || v <= 0 {
			return cfg, fmt.Errorf("invalid bandwidth value %q", part)
		}
		cfg.bandwidth = append(cfg.bandwidth, v)
	}
	if len(cfg.bandwidth) == 0 {
		return cfg, fmt.Errorf("bandwidth trace is empty")
	}
	return cfg, nil
}

func printTextSummary(s *report.Summary) {
	fmt.Printf("assets: %d processed, %d failed\n", len(s.Assets), s.AssetsFailed)
	for _, a := range s.Assets {
		fmt.Printf("  %-20s %-9s segments=%d dup=%d conflicts=%d tracks=%d av_p95=%.1fms %.1fms\n",
			a.Name, a.Status, a.Segments, a.Duplicates, a.Conflicts, a.Tracks, a.AVSyncP95Ms, a.ProcessingMs)
	}
	fmt.Printf("av_sync_error_ms: p50=%.2f p95=%.2f max=%.2f\n", s.AVSyncErrorMs.P50, s.AVSyncErrorMs.P95, s.AVSyncErrorMs.Max)
	fmt.Printf("packet_recovery_rate: %.4f\n", s.PacketRecoveryRate)
	fmt.Printf("processing_p95_ms: %.1f\n", s.ProcessingP95Ms)
	fmt.Printf("checkpoint_resume_verified: %v\n", s.CheckpointResumeVerified)
	fmt.Printf("corrupt_cases_detected: %d/%d\n", s.CorruptCasesDetected, s.CorruptCasesTotal)
}
