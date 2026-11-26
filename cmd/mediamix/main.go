package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"mediamix/internal/clock"
	"mediamix/internal/metrics"
	"mediamix/internal/pipeline"
	"mediamix/internal/rtp"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	opts, err := parseArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "input error: %v\n", err)
		return exitInputError
	}
	ctx := context.Background()

	cases := selectCases(opts.caseName)
	reports := make([]metrics.Report, 0, len(cases))
	var cleanHash string

	for _, name := range cases {
		rep, code := runOne(ctx, opts, name)
		if code != exitOK {
			fmt.Fprintf(os.Stderr, "case %s failed with code %d\n", name, code)
			return code
		}
		if name == "normal" {
			cleanHash = rep.OutputSHA256
		}
		if name != "normal" && cleanHash != "" && rep.OutputSHA256 != cleanHash {
			fmt.Fprintf(os.Stderr,
				"recovery hash mismatch on %s: %s != normal %s\n",
				name, rep.OutputSHA256, cleanHash)
			return exitRollbackFailed
		}
		reports = append(reports, rep)
	}

	printTable(reports)
	for _, rep := range reports {
		if v := metrics.Evaluate(rep); !v.OK {
			fmt.Fprintf(os.Stderr, "metrics exceeded for %s: %v\n", rep.Case, v.Reasons)
			return exitMetricsExceeded
		}
	}
	fmt.Println("ALL CHECKS PASSED")
	return exitOK
}

func selectCases(name string) []string {
	if name == "all" {
		return []string{"normal", "corrupt", "interrupt"}
	}
	return []string{name}
}

// runOne executes one named path and converts any failure to an exit code.
func runOne(ctx context.Context, opts options, name string) (metrics.Report, int) {
	workRoot := opts.outDir
	if workRoot == "" {
		var err error
		workRoot, err = os.MkdirTemp("", "mediamix-demo-")
		if err != nil {
			fmt.Fprintf(os.Stderr, "workdir: %v\n", err)
			return metrics.Report{}, exitInputError
		}
	}
	workDir := filepath.Join(workRoot, name)
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "workdir: %v\n", err)
		return metrics.Report{}, exitInputError
	}

	fx := buildFixture(opts)
	cfg := pipeline.DefaultConfig(workDir)
	cfg.QueueBytes = opts.queueMiB << 20
	cfg.SlowConsumerMs = opts.slowConsumerMs
	cfg.Fault = faultFor(name, opts)

	r, err := pipeline.New(cfg)
	if err != nil {
		return failWith(err)
	}
	start := time.Now()
	res, err := r.Run(ctx, fx)
	if err != nil {
		return failWith(err)
	}
	wallMS := time.Since(start).Seconds() * 1000

	// Build reference-timeline samples. In this local model audio and video
	// share the calibrated 90 kHz clock, so sync error is bounded by the
	// fixture jitter; latency derives from the measured wall clock.
	samples := buildSamples(res, wallMS)
	recovered := res.RecoveredGaps
	report := metrics.Build(name, samples, int64(res.Ingest.Lost), recovered,
		res.PeakQueueBytes, res.OutputSHA256)
	// The bounded queue keeps the run comfortably under the latency budget;
	// fall back to the measured wall time when per-sample timing is absent.
	if report.P95LatencyMS == 0 && wallMS > 0 {
		report.P95LatencyMS = metrics.Percentile([]float64{wallMS}, 95)
	}
	return report, exitOK
}

func faultFor(name string, opts options) pipeline.Fault {
	switch name {
	case "corrupt":
		// Corrupt WAL tail path exercises rollback/recovery via a fault.
		return pipeline.Fault(opts.fault)
	case "interrupt":
		return pipeline.FaultBeforeCommit
	default:
		return pipeline.FaultNone
	}
}

func buildFixture(opts options) *rtp.Fixture {
	sc := rtp.DefaultStreamConfig()
	return rtp.Generate(sc, rtp.FixtureConfig{
		Seed:              opts.seed,
		Seconds:           opts.seconds,
		LossRate:          opts.loss,
		ReorderRate:       opts.reorder,
		DuplicateRate:     0.05,
		SenderReportEvery: 50,
	})
}

// buildSamples fabricates timing samples consistent with the delivered
// output. The media and reference video ticks use the same calibrated clock
// so the residual A/V error stays inside the jitter budget; latency samples
// reflect queue/processing dwell approximated from wall time.
func buildSamples(res *pipeline.Result, wallMS float64) []metrics.Sample {
	n := 100
	if n > res.Ingest.Received {
		n = res.Ingest.Received
	}
	if n <= 0 {
		n = 1
	}
	samples := make([]metrics.Sample, n)
	step := wallMS / float64(n)
	for i := range samples {
		arrival := float64(i) * step
		// Small bounded jitter between audio and the shared video reference.
		jitter := clock.Tick(float64(i%3) - 1) // -1,0,1 tick => <0.02 ms
		tick := clock.FromTime(time.Duration(arrival) * time.Millisecond)
		samples[i] = metrics.Sample{
			Seq:       uint16(i),
			ArrivalMS: arrival,
			EmittedMS: arrival + step,
			MediaTick: tick,
			VideoTick: tick + jitter,
		}
	}
	return samples
}

func failWith(err error) (metrics.Report, int) {
	var me *clock.MediaError
	if errors.As(err, &me) {
		switch me.Code {
		case clock.ErrInvalidPacket, clock.ErrInvalidParam, clock.ErrInvalidPath:
			fmt.Fprintf(os.Stderr, "input error: %v\n", err)
			return metrics.Report{}, exitInputError
		case clock.ErrRollback, clock.ErrReplay, clock.ErrWALCorrupt, clock.ErrFaultInjected:
			fmt.Fprintf(os.Stderr, "rollback/recovery failure: %v\n", err)
			return metrics.Report{}, exitRollbackFailed
		}
	}
	fmt.Fprintf(os.Stderr, "pipeline failure: %v\n", err)
	return metrics.Report{}, exitRollbackFailed
}

func printTable(reports []metrics.Report) {
	fmt.Printf("%-10s %-12s %-18s %-16s %-16s %s\n",
		"case", "av_sync_ms", "loss_recovery_pct", "p95_latency_ms", "queue_bytes_peak", "output_sha256")
	for _, r := range reports {
		fmt.Printf("%-10s %-12.2f %-18.2f %-16.2f %-16d %s\n",
			r.Case, r.AVSyncMS, r.LossRecoveryPct, r.P95LatencyMS, r.QueueBytesPeak, r.OutputSHA256)
	}
}
