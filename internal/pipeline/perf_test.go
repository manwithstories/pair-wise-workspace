package pipeline

import (
	"testing"
	"time"
)

func TestPerformanceTenMinuteSample(t *testing.T) {
	if testing.Short() {
		t.Skip("performance test skipped in short mode")
	}
	dir := t.TempDir()
	// 10 minutes: 150 segments of 4s, three cameras.
	cfg := Config{
		Seed: 42, Cameras: []string{"cam0", "cam1", "cam2"},
		Segments: 150, SegmentDurNS: 4_000_000_000, PacketRate: 50,
		WorkDir: dir,
		Faults:  Faults{Reorder: true, Truncate: true, Drift: true},
	}
	start := time.Now()
	res, err := Run(cfg)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	t.Logf("10-minute sample took %s; segments=%d keyframes=%d", elapsed, res.Segments, res.Keyframes)
	if elapsed > 2*time.Minute {
		t.Fatalf("exceeded 2-minute budget: %s", elapsed)
	}
	if res.SegmentHashMatch < 1.0 {
		t.Fatalf("segment hash match = %v", res.SegmentHashMatch)
	}
}
