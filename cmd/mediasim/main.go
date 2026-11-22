// mediasim is the demonstration entry point for the media kernel. It only
// generates inputs, runs the reusable pipeline packages and prints metrics;
// all processing logic lives in internal/.
//
// Exit codes:
//
//	0  success
//	2  corrupted input or scenario metric threshold exceeded
//	64 invalid command-line parameters (EX_USAGE convention)
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"mediakernel/internal/model"
	"mediakernel/internal/pipeline"
	"mediakernel/internal/sim"
)

// ExitCodeInputFailure is returned for malformed input or failed thresholds.
const (
	ExitOK           = 0
	ExitMetricFailed = 2
	ExitUsage        = 64
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fs := flag.NewFlagSet("mediasim", flag.ContinueOnError)
	scenario := fs.String("scenario", "", "nominal|corrupt|recovery|burst")
	seed := fs.Int64("seed", 41, "deterministic trace seed")
	report := fs.String("report", "stdout", "report destination (stdout)")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if *scenario == "" || !sim.ValidScenario(*scenario) {
		fmt.Fprintf(os.Stderr, "invalid -scenario %q; choose nominal, corrupt, recovery or burst\n", *scenario)
		return ExitUsage
	}
	if *report != "stdout" {
		fmt.Fprintf(os.Stderr, "invalid -report %q; only stdout is supported\n", *report)
		return ExitUsage
	}
	if *seed < 0 {
		fmt.Fprintln(os.Stderr, "invalid -seed: must be non-negative")
		return ExitUsage
	}

	input, trace, err := generateInput(*scenario, *seed)
	if err != nil {
		fmt.Fprintf(os.Stderr, "input generation failed: %v\n", err)
		return ExitMetricFailed
	}
	engine := pipeline.New(pipeline.DefaultConfig(*scenario))
	out := engine.Run(input)

	// Container-level timestamp verification is part of the run contract.
	if err := verifyVideoClip(input.VideoFrames); err != nil {
		fmt.Fprintf(os.Stderr, "mp4 timestamp verification failed: %v\n", err)
		return ExitMetricFailed
	}

	th := scenarioThresholds(*scenario)
	failure := evaluate(*scenario, out.Metrics, th)
	reportLine := buildReport(out.Metrics)
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(reportLine); err != nil {
		fmt.Fprintf(os.Stderr, "report encoding failed: %v\n", err)
		return ExitMetricFailed
	}
	if failure != "" {
		fmt.Fprintf(os.Stderr, "scenario threshold failed: %s (trace had %d lost packets)\n",
			failure, len(trace.LostSeqs))
		return ExitMetricFailed
	}
	return ExitOK
}

// reportRow exposes exactly the acceptance fields, in the required names.
type reportRow struct {
	Scenario             string  `json:"scenario"`
	AVSyncMSP95          int64   `json:"av_sync_ms_p95"`
	LossRecoveryRate     float64 `json:"loss_recovery_rate"`
	LatencyMSP95         int64   `json:"latency_ms_p95"`
	DuplicateCount       int     `json:"duplicate_count"`
	ReorderedCount       int     `json:"reordered_count"`
	LateReplayCount      int     `json:"late_replay_count"`
	UnrecoverableSamples int     `json:"unrecoverable_samples"`
	ResultHash           string  `json:"result_hash"`
}

func buildReport(m model.Metrics) reportRow {
	return reportRow{
		Scenario:             m.Scenario,
		AVSyncMSP95:          m.AVSyncMSP95,
		LossRecoveryRate:     m.LossRecoveryRate,
		LatencyMSP95:         m.LatencyMSP95,
		DuplicateCount:       m.DuplicateCount,
		ReorderedCount:       m.ReorderedCount,
		LateReplayCount:      m.LateReplayCount,
		UnrecoverableSamples: m.UnrecoverableSamples,
		ResultHash:           m.ResultHash,
	}
}
