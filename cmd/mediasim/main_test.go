package main

import (
	"encoding/json"
	"testing"

	"mediakernel/internal/model"
	"mediakernel/internal/pipeline"
	"mediakernel/internal/sim"
)

func TestRunAllScenariosExitZero(t *testing.T) {
	for _, sc := range []string{"nominal", "corrupt", "recovery", "burst"} {
		if code := run([]string{"-scenario", sc, "-seed", "41", "-report", "stdout"}); code != ExitOK {
			t.Fatalf("%s: exit code %d want 0", sc, code)
		}
	}
}

func TestDeterministicResultHashSameSeed(t *testing.T) {
	hashes := map[string]string{}
	for _, sc := range []string{"nominal", "corrupt", "recovery", "burst"} {
		first := runCapture(t, sc, 41)
		second := runCapture(t, sc, 41)
		if first.ResultHash != second.ResultHash {
			t.Fatalf("%s: result hash not reproducible %s vs %s", sc, first.ResultHash, second.ResultHash)
		}
		if first.ResultHash == "" {
			t.Fatalf("%s: empty result hash", sc)
		}
		hashes[sc] = first.ResultHash
	}
	// Distinct scenarios must not collapse onto one hash.
	seen := map[string]bool{}
	for _, h := range hashes {
		if seen[h] {
			t.Fatalf("scenarios share a result hash: %s", h)
		}
		seen[h] = true
	}
}

func TestCorruptReportsUnrecoverable(t *testing.T) {
	row := runCapture(t, "corrupt", 41)
	if row.UnrecoverableSamples == 0 {
		t.Fatal("corrupt scenario must report non-zero unrecoverable samples")
	}
}

func TestRecoveryRateThreshold(t *testing.T) {
	row := runCapture(t, "recovery", 41)
	if row.LossRecoveryRate < 0.8 {
		t.Fatalf("recovery rate below 0.8: %v", row.LossRecoveryRate)
	}
}

func TestNominalThresholds(t *testing.T) {
	row := runCapture(t, "nominal", 41)
	if row.AVSyncMSP95 > 20 {
		t.Fatalf("nominal av sync p95 %d > 20", row.AVSyncMSP95)
	}
	if row.LatencyMSP95 > 180 {
		t.Fatalf("nominal latency p95 %d > 180", row.LatencyMSP95)
	}
}

func TestUsageExitCode64(t *testing.T) {
	cases := [][]string{
		nil,
		{"-scenario", "bogus"},
		{"-scenario", "nominal", "-report", "file"},
		{"-scenario", "nominal", "-seed", "-3"},
	}
	for _, args := range cases {
		if code := run(args); code != ExitUsage {
			t.Fatalf("args %v: exit %d want %d", args, code, ExitUsage)
		}
	}
}

func TestThresholdFailureExitCode2(t *testing.T) {
	// Install an unreachable nominal gate, run the real end-to-end run() path
	// and confirm it exits 2, then restore the production table.
	orig := thresholdsByScenario[sim.ScenarioNominal]
	thresholdsByScenario[sim.ScenarioNominal] = threshold{maxAVSyncMS: 0, maxLatencyMS: -1}
	defer func() { thresholdsByScenario[sim.ScenarioNominal] = orig }()

	code := run([]string{"-scenario", "nominal", "-seed", "41", "-report", "stdout"})
	if code != ExitMetricFailed {
		t.Fatalf("threshold breach must exit %d, got %d", ExitMetricFailed, code)
	}

	// The pure evaluate() helper must agree with the run() decision.
	row := runCapture(t, "recovery", 41)
	th := scenarioThresholds("recovery")
	th.minRecovery = 1.5
	if msg := evaluate("recovery", metricsFromRow(row), th); msg == "" {
		t.Fatal("impossible recovery minimum must produce a threshold failure")
	}
}

// runCapture executes the same deterministic input path as run() and returns
// the report row without depending on captured stdout.
func runCapture(t *testing.T, scenario string, seed int64) reportRow {
	t.Helper()
	input, _, err := generateInput(scenario, int64(seed))
	if err != nil {
		t.Fatal(err)
	}
	out := pipeline.New(pipeline.DefaultConfig(scenario)).Run(input)
	return buildReport(out.Metrics)
}

// metricsFromRow rebuilds the metric fields evaluate() inspects.
func metricsFromRow(r reportRow) model.Metrics {
	return model.Metrics{
		Scenario:             r.Scenario,
		AVSyncMSP95:          r.AVSyncMSP95,
		LossRecoveryRate:     r.LossRecoveryRate,
		LatencyMSP95:         r.LatencyMSP95,
		DuplicateCount:       r.DuplicateCount,
		ReorderedCount:       r.ReorderedCount,
		LateReplayCount:      r.LateReplayCount,
		UnrecoverableSamples: r.UnrecoverableSamples,
		ResultHash:           r.ResultHash,
	}
}

// jsonRoundTrip ensures the report field names match the acceptance contract.
func TestReportFieldNames(t *testing.T) {
	row := runCapture(t, "nominal", 41)
	raw, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	required := []string{
		"scenario", "av_sync_ms_p95", "loss_recovery_rate", "latency_ms_p95",
		"duplicate_count", "reordered_count", "late_replay_count",
		"unrecoverable_samples", "result_hash",
	}
	for _, key := range required {
		if _, ok := decoded[key]; !ok {
			t.Fatalf("report missing required field %q", key)
		}
	}
}
