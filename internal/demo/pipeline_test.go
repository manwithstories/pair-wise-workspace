package demo

import (
	"context"
	"testing"
	"time"

	"github.com/clearliu/fieldcut/internal/clock"
	"github.com/clearliu/fieldcut/internal/fixtures"
)

func shortOptions(seed int64) Options {
	return Options{
		Seed:           seed,
		Duration:       20 * time.Second,
		QueueBytes:     1 << 20,
		QueueItems:     256,
		LateWindow:     4 * time.Second,
		InjectLoss:     0.02,
		SlowConsumerMs: 5,
		Case:           fixtures.CaseNormal,
	}
}

func TestRunNormalMeetsThresholds(t *testing.T) {
	m, err := Run(context.Background(), shortOptions(41))
	if err != nil {
		t.Fatal(err)
	}
	if m.AVSyncP95Ms > 20 {
		t.Fatalf("av_sync_ms_p95=%d > 20", m.AVSyncP95Ms)
	}
	if m.PacketRecoveryRate < 0.95 {
		t.Fatalf("packet_recovery_rate=%.3f < 0.95", m.PacketRecoveryRate)
	}
	if m.PipelineLatencyP95Ms > 500 {
		t.Fatalf("pipeline_latency_p95_ms=%d > 500", m.PipelineLatencyP95Ms)
	}
	if m.PeakBufferBytes > 1<<20 {
		t.Fatalf("peak_buffer_bytes=%d exceeds budget", m.PeakBufferBytes)
	}
	if m.IndexSeekMisses != 0 {
		t.Fatalf("index_seek_misses=%d, want 0", m.IndexSeekMisses)
	}
	if len(m.IndexSHA256) != 64 {
		t.Fatalf("index_sha256 malformed: %q", m.IndexSHA256)
	}
}

func TestRerunDeterminism(t *testing.T) {
	first, err := Run(context.Background(), shortOptions(41))
	if err != nil {
		t.Fatal(err)
	}
	second, err := Run(context.Background(), shortOptions(41))
	if err != nil {
		t.Fatal(err)
	}
	if first.IndexSHA256 != second.IndexSHA256 {
		t.Fatalf("digest drift across reruns: %s != %s", first.IndexSHA256, second.IndexSHA256)
	}
	if first.ProcessedPackets != second.ProcessedPackets ||
		first.GapPackets != second.GapPackets ||
		first.LateReplayCount != second.LateReplayCount {
		t.Fatal("counter drift across reruns")
	}
}

func TestTruncatedBoxIsolationAcrossModules(t *testing.T) {
	opts := shortOptions(41)
	opts.Case = fixtures.CaseTruncatedBox
	m, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("truncated box must be isolated, not fatal: %v", err)
	}
	// The corrupted fragment is quarantined by the ingest stage and the
	// index still builds with zero seek misses.
	if m.Quarantined == 0 {
		t.Fatal("truncated box was not quarantined")
	}
	if m.IndexSeekMisses != 0 {
		t.Fatalf("index seek misses after isolation: %d", m.IndexSeekMisses)
	}
	// Rerun stability with corruption present.
	again, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if again.IndexSHA256 != m.IndexSHA256 || again.Quarantined != m.Quarantined {
		t.Fatal("truncated-box handling non-deterministic across runs")
	}
}

func TestLateOutsideWindowBoundary(t *testing.T) {
	// A 1s window guarantees the injected late event lands outside it
	// (the late frame sits roughly a quarter into the capture).
	opts := shortOptions(41)
	opts.Case = fixtures.CaseLateOutsideWindow
	opts.LateWindow = time.Second
	m, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if m.Quarantined == 0 {
		t.Fatal("late event outside window must be reported/quarantined")
	}
	// The same late event inside a generous window must be replayed
	// instead of isolated.
	opts.LateWindow = 30 * time.Second
	inWindow, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if inWindow.LateReplayCount == 0 {
		t.Fatalf("in-window late event should trigger replay: %+v", inWindow)
	}
}

func TestHardLimitsRejectBadOptions(t *testing.T) {
	cases := map[string]func(*Options){
		"zero queue bytes":  func(o *Options) { o.QueueBytes = 0 },
		"loss >= 1":         func(o *Options) { o.InjectLoss = 1 },
		"negative duration": func(o *Options) { o.Duration = -time.Second },
		"oversize window": func(o *Options) {
			o.LateWindow = clock.Ticks(clock.MaxLateWindowTicks).Duration() + time.Second
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			opts := shortOptions(7)
			mutate(&opts)
			if _, err := Run(context.Background(), opts); err == nil {
				t.Fatalf("%s: expected rejection", name)
			}
		})
	}
}

func TestPeakBufferNeverExceedsBudget(t *testing.T) {
	// Tiny budgets force continuous backpressure; memory must stay bounded.
	opts := shortOptions(41)
	opts.QueueBytes = 4096
	opts.QueueItems = 8
	m, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if m.PeakBufferBytes > 4096 {
		t.Fatalf("peak %d exceeded hard budget 4096", m.PeakBufferBytes)
	}
}
