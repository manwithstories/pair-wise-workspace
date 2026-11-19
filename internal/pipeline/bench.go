package pipeline

import (
	"time"

	"archivearchivist/internal/model"
	"archivearchivist/internal/report"
)

// BenchResult is one benchmark run over a fixed fixture.
type BenchResult struct {
	Analysis      *Analysis      `json:"-"`
	Iterations    int            `json:"iterations"`
	LatencyP95US  int64          `json:"latency_p95_us"`
	LatencyMeanUS int64          `json:"latency_mean_us"`
	LatencyMaxUS  int64          `json:"latency_max_us"`
	SyncP95US     int64          `json:"sync_error_p95_us"`
	SyncMaxUS     int64          `json:"sync_error_max_us"`
	RecoveryRate  model.Rational `json:"packet_recovery_rate"`
	Wraps         int            `json:"wraps"`
	ConstantFPS   bool           `json:"constant_fps"`
	CorruptRanges int            `json:"corrupt_ranges"`
	IterationMS   []int64        `json:"iteration_ms"`
}

// Benchmark runs the analysis at least minIterations times and reports
// end-to-end sync error, packet recovery and latency percentiles. Every
// iteration re-reads the inputs, so the latency includes parsing and scanning
// rather than only the in-memory planner.
func Benchmark(in Input, opts Options, minIterations int) (*BenchResult, error) {
	if minIterations < 30 {
		minIterations = 30
	}
	lats := make([]int64, 0, minIterations)
	var last *Analysis
	for i := 0; i < minIterations; i++ {
		start := time.Now()
		a, err := Run(in, opts)
		elapsed := time.Since(start)
		if err != nil {
			return nil, err
		}
		lats = append(lats, elapsed.Microseconds())
		last = a
	}
	ms := make([]int64, len(lats))
	for i, v := range lats {
		ms[i] = v / 1000
	}
	res := &BenchResult{
		Iterations:    minIterations,
		LatencyP95US:  report.Quantile(lats, 95),
		LatencyMeanUS: report.Mean(lats),
		LatencyMaxUS:  report.Quantile(lats, 100),
		SyncP95US:     last.Metrics.SyncErrorP95US,
		SyncMaxUS:     last.Metrics.SyncErrorMaxUS,
		RecoveryRate:  last.Metrics.PacketRecoveryRate,
		Wraps:         last.Metrics.Wraps,
		ConstantFPS:   last.Metrics.ConstantFPS,
		CorruptRanges: last.Metrics.CorruptRanges,
		IterationMS:   ms,
	}
	// Fold the latency figures into the analysis metrics so the JSON written
	// by any command carries the same acceptance numbers.
	last.Metrics.Iterations = minIterations
	last.Metrics.LatencyP95US = res.LatencyP95US
	last.Metrics.LatencyMeanUS = res.LatencyMeanUS
	res.Analysis = last
	return res, nil
}

// BenchMarks is the JSON document written by the bench command.
type BenchMarks struct {
	Command string          `json:"command"`
	Bench   *BenchResult    `json:"bench"`
	Metrics report.Metrics  `json:"metrics"`
	Faults  model.FaultList `json:"faults"`
}
