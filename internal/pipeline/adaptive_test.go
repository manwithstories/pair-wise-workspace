package pipeline

import (
	"testing"

	"mediamix/internal/timeline"
)

func TestTunerHoldsSteadyOnUniformArrivals(t *testing.T) {
	tuner := newBandwidthTuner(3)
	// Perfectly paced arrivals: one packet every 10 ms.
	for i := 0; i < 200; i++ {
		mbps, changed := tuner.observe(timeline.Micros(i) * 10_000)
		if changed && (mbps < 2 || mbps > 4) {
			t.Fatalf("steady arrivals drifted the estimate to %.2f", mbps)
		}
	}
}

func TestTunerReportsDropWhenArrivalsStretch(t *testing.T) {
	tuner := newBandwidthTuner(4)
	// First half paced at 5 ms, second half stalls to 25 ms: the link slowed.
	now := timeline.Micros(0)
	sawChange := false
	var lastMBps float64
	for i := 0; i < 400; i++ {
		step := timeline.Micros(5_000)
		if i > 200 {
			step = timeline.Micros(25_000)
		}
		now += step
		mbps, changed := tuner.observe(now)
		if changed {
			sawChange = true
			lastMBps = mbps
		}
	}
	if !sawChange {
		t.Fatal("a sustained slowdown must trigger a re-sample")
	}
	if lastMBps >= 4 {
		t.Fatalf("estimate did not fall: %.2f", lastMBps)
	}
}

func TestTunerClampsExtremes(t *testing.T) {
	tuner := newBandwidthTuner(2)
	now := timeline.Micros(0)
	// Extreme stalls must not drive the estimate to zero.
	for i := 0; i < 400; i++ {
		now += timeline.Micros(1_000_000)
		mbps, changed := tuner.observe(now)
		if changed && mbps < 0.5 {
			t.Fatalf("estimate collapsed to %.3f", mbps)
		}
	}
}

func TestAdaptiveRunMeetsContract(t *testing.T) {
	rep, err := Run(Options{Seed: 41, LossBurst: 7, BandwidthMBps: 3})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Timeouts != 0 {
		t.Fatalf("a healthy 3 Mbps link should not time out gaps: %d", rep.Timeouts)
	}
	if r := rep.RecoveryRate; r < 0.90 {
		t.Fatalf("recovery %.3f", r)
	}
	if rep.ExitCode != 0 {
		t.Fatalf("exit = %d", rep.ExitCode)
	}
}

func TestStressedLinkReportsTimeoutsAndExitFour(t *testing.T) {
	rep, err := Run(Options{Seed: 41, LossBurst: 7, BandwidthMBps: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Unrecoverable) == 0 {
		t.Fatal("a 1 Mbps link cannot heal a 7-packet burst per track")
	}
	if rep.ExitCode != 4 {
		t.Fatalf("exit = %d, want 4", rep.ExitCode)
	}
	if rep.LatencyP95MS > 500 {
		t.Fatalf("latency must stay bounded: %.1f ms", rep.LatencyP95MS)
	}
}
