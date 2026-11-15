package runner

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestRunnerGoroutinesStayFlat(t *testing.T) {
	dir := t.TempDir()
	opts := Options{
		Duration: 250 * time.Millisecond, PPS: 200, Seed: 7,
		Timeout: 5 * time.Second, ClockRate: 48000,
		Out: filepath.Join(dir, "warmup.json"),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if result := Run(ctx, opts); result.ExitCode != 0 {
		t.Fatalf("warmup exit=%d error=%s", result.ExitCode, result.Report.Error)
	}
	time.Sleep(250 * time.Millisecond)
	baseline := runtime.NumGoroutine()
	for i := 0; i < 2; i++ {
		opts.Out = filepath.Join(dir, "run.json")
		if result := Run(ctx, opts); result.ExitCode != 0 {
			t.Fatalf("run %d exit=%d error=%s", i, result.ExitCode, result.Report.Error)
		}
		time.Sleep(250 * time.Millisecond)
		if current := runtime.NumGoroutine(); current > baseline+2 {
			t.Fatalf("run %d goroutines grew from %d to %d", i, baseline, current)
		}
	}
}
