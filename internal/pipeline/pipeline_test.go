package pipeline

import (
	"context"
	"math"
	"testing"

	"avpipe/internal/fault"
	"avpipe/internal/report"
	"avpipe/internal/track"
)

func TestRunNormalSamplePasses(t *testing.T) {
	opts := Options{Seed: 42, Loss: 0.08, JitterMs: 15}
	rep := Run(context.Background(), opts)
	if rep.ExitCode != 0 || !rep.Passed {
		t.Fatalf("normal run failed: exit=%d metrics=%+v", rep.ExitCode, rep.Metrics)
	}
	if rep.SyncErrorMs >= 20 {
		t.Fatalf("sync p95 %v exceeds 20ms", rep.SyncErrorMs)
	}
	if rep.LatencyP95Ms >= 120 {
		t.Fatalf("latency p95 %v exceeds 120ms", rep.LatencyP95Ms)
	}
	if rep.PacketRecovery < 0.9 {
		t.Fatalf("recovery %.3f below 0.9", rep.PacketRecovery)
	}
	if rep.QueuePeakBytes > 262144 {
		t.Fatalf("queue peak %d exceeds cap", rep.QueuePeakBytes)
	}
	if rep.RMSBefore <= 0 || rep.RMSBefore == rep.RMSAfter {
		t.Fatalf("loudness not measured/applied: %+v", rep)
	}
	if rep.SilenceFills != 0 {
		t.Fatalf("5.1->stereo should fill nothing, got %d", rep.SilenceFills)
	}
}

func TestRunSlowConsumerProfile(t *testing.T) {
	opts := Options{Seed: 3, Loss: 0.08, JitterMs: 10, ConsumerDelayMs: 180}
	rep := Run(context.Background(), opts)
	if rep.Profile != "slow-consumer" {
		t.Fatalf("profile=%s", rep.Profile)
	}
	if !rep.Passed {
		t.Fatalf("slow-consumer run failed: %+v", rep.Metrics)
	}
	if rep.LatencyP95Ms < 150 || rep.LatencyP95Ms >= 250 {
		t.Fatalf("latency p95 %v not in slow-consumer band", rep.LatencyP95Ms)
	}
}

func TestRunDriftBoundaryProfile(t *testing.T) {
	opts := Options{Seed: 5, Loss: 0.08, JitterMs: 60, DriftPPM: 25000}
	rep := Run(context.Background(), opts)
	if rep.Profile != "drift-jitter-boundary" {
		t.Fatalf("profile=%s", rep.Profile)
	}
	if !rep.Passed || rep.SyncErrorMs >= 80 {
		t.Fatalf("boundary failed: sync=%v exit=%d", rep.SyncErrorMs, rep.ExitCode)
	}
	if !rep.DriftResolvable {
		t.Fatalf("a 25000ppm drift must be resolvable (stderr %v)", rep.DriftStdErrPPM)
	}
	if math.Abs(rep.DriftEstimatePPM-25000) > 500 {
		t.Fatalf("drift estimate %v far from 25000", rep.DriftEstimatePPM)
	}
}

func TestRunDeterministic(t *testing.T) {
	opts := Options{Seed: 42, Loss: 0.08, JitterMs: 35, ConsumerDelayMs: 18}
	a := Run(context.Background(), opts)
	b := Run(context.Background(), opts)
	ab, _ := a.Encode()
	bb, _ := b.Encode()
	if string(ab) != string(bb) {
		t.Fatal("same seed produced different reports")
	}
}

func TestRunResamplesWhenRatesDiffer(t *testing.T) {
	opts := Options{Seed: 1, SourceRate: 44100, TargetRate: 48000}
	rep := Run(context.Background(), opts)
	if !rep.Passed {
		t.Fatalf("resample run failed: %+v", rep.Metrics)
	}
	if rep.RoundingError < 0 || rep.RoundingError >= 1 {
		t.Fatalf("rounding error %v outside one sample", rep.RoundingError)
	}
}

func TestRunFillSilenceWhenUpscalingLayout(t *testing.T) {
	opts := Options{Seed: 1, SourceLayout: "stereo", TargetLayout: "5.1"}
	rep := Run(context.Background(), opts)
	if rep.ExitCode != 0 {
		t.Fatalf("upmap failed: %+v", rep.RootError)
	}
	if rep.SilenceFills != 4 {
		t.Fatalf("stereo->5.1 should fill 4 channels, got %d", rep.SilenceFills)
	}
}

func TestRunRejectsBadLayoutWithInputExit(t *testing.T) {
	opts := Options{Seed: 1, TargetLayout: "atmos"}
	rep := Run(context.Background(), opts)
	if rep.ExitCode != 2 {
		t.Fatalf("exit=%d want 2 (%+v)", rep.ExitCode, rep.RootError)
	}
	if rep.RootError == nil || rep.RootError.Code != string(fault.CodeUnsupportedLayout) {
		t.Fatalf("root=%+v", rep.RootError)
	}
}

func TestRunHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rep := Run(ctx, Options{Seed: 1})
	if rep.ExitCode != 4 {
		t.Fatalf("canceled run exit=%d", rep.ExitCode)
	}
	if rep.RootError == nil {
		t.Fatal("canceled run has no root error")
	}
}

func TestRunReportLeaksNoSamples(t *testing.T) {
	rep := Run(context.Background(), Options{Seed: 1})
	buf, err := rep.Encode()
	if err != nil {
		t.Fatal(err)
	}
	// The report may mention a sample count, but never sample values.
	for _, banned := range []string{"\"data\":", "\"payload\"", "\"pcm\""} {
		if contains(string(buf), banned) {
			t.Fatalf("report contains %s", banned)
		}
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}

func TestTrackOutcomeCancelsDependentsAndKeepsRoot(t *testing.T) {
	g := track.New()
	g.MustAdd(track.Node{Name: "audio", Kind: track.KindAudio})
	g.MustAdd(track.Node{Name: "video", Kind: track.KindVideo, Deps: []string{"audio"}})
	g.MustAdd(track.Node{Name: "report", Kind: track.KindDerived, Deps: []string{"video"}})
	root := fault.New(fault.CodeWAVLength, "audio.parse", "truncated").At(44)
	out, err := g.Run(context.Background(), func(ctx context.Context, name string) error {
		if name == "audio" {
			return root
		}
		return nil
	})
	if err == nil || fault.Root(err).Code != fault.CodeWAVLength {
		t.Fatalf("root cause lost: %v", err)
	}
	if out.Status["video"] != track.StatusCanceled || out.Status["report"] != track.StatusCanceled {
		t.Fatalf("dependents not canceled: %v", out.Status)
	}
	statuses := toTrackStatus(out)
	if len(statuses) != 3 || statuses[0].Name != "audio" || statuses[0].Status != "failed" {
		t.Fatalf("statuses=%+v", statuses)
	}
	if statuses[0].Error == "" {
		t.Fatal("failed track has no error text")
	}
}

func TestProfileSelection(t *testing.T) {
	cases := []struct {
		opts Options
		want Profile
	}{
		{Options{}, ProfileNormal},
		{Options{JitterMs: 35}, ProfileBoundary},
		{Options{DriftPPM: 5000}, ProfileBoundary},
		{Options{ConsumerDelayMs: 150}, ProfileSlowConsumer},
	}
	for _, c := range cases {
		got, _ := ProfileFor(c.opts)
		if got != c.want {
			t.Fatalf("%+v -> %s want %s", c.opts, got, c.want)
		}
	}
}

func TestPacingSteadyStateHasNoBacklog(t *testing.T) {
	p := Pacing{BlockMs: 20}
	// Arrivals exactly on the block cadence keep up: latency is bounded by
	// one block (the consumer is always finishing the previous one) and
	// never grows, and nothing is dropped.
	var arrivals []float64
	for i := 0; i < 50; i++ {
		arrivals = append(arrivals, float64(i)*20)
	}
	lat, _, dropped := p.Simulate(16, "block", arrivals)
	if dropped != 0 {
		t.Fatalf("dropped %d", dropped)
	}
	for _, l := range lat {
		if l > 20.001 {
			t.Fatalf("steady-state latency grew to %v", l)
		}
	}
	if n := len(lat); n != len(arrivals) {
		t.Fatalf("measured %d of %d blocks", n, len(arrivals))
	}
}

func TestPacingDropPolicyRejects(t *testing.T) {
	p := Pacing{BlockMs: 100}
	var arrivals []float64
	for i := 0; i < 20; i++ {
		arrivals = append(arrivals, 0) // all at once: deeper than capacity
	}
	_, _, dropped := p.Simulate(4, "drop", arrivals)
	if dropped == 0 {
		t.Fatal("drop policy admitted everything")
	}
}

func TestMetricsForThresholdFailure(t *testing.T) {
	rep := &report.Report{SyncErrorMs: 90, LatencyP95Ms: 10, PacketRecovery: 1, QueuePeakBytes: 1}
	th := report.Thresholds{SyncP95Ms: 80, LatencyP95Ms: 120, RecoveryRateMin: 0.9,
		QueuePeakMax: 262144}
	ms := metricsFor(th, rep)
	failed := 0
	for _, m := range ms {
		if m.Checked && !m.Passed {
			failed++
		}
	}
	if failed != 1 {
		t.Fatalf("failed=%d want 1: %+v", failed, ms)
	}
}
