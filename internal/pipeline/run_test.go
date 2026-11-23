package pipeline

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"mediacore/internal/fixture"
)

func baseCfg(t *testing.T, mode fixture.Mode, name string) Config {
	dir := t.TempDir()
	cfg := DefaultConfig(42, 4,
		filepath.Join(dir, name+".json"),
		filepath.Join(dir, name+".mp4"))
	cfg.Mode = mode
	return cfg
}

func TestRunHealthy(t *testing.T) {
	cfg := baseCfg(t, fixture.ModeOK, "ok")
	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "ok" || res.Tracks < 3 || res.Samples == 0 {
		t.Fatalf("unexpected result: %+v", res)
	}
	if !res.OrderedEqual || res.SHA256 == "" {
		t.Fatalf("ordering/checksum wrong: %+v", res)
	}
}

func TestRerunChecksumStable(t *testing.T) {
	cfg1 := baseCfg(t, fixture.ModeOK, "a")
	res1, err := Run(context.Background(), cfg1)
	if err != nil {
		t.Fatal(err)
	}
	cfg2 := baseCfg(t, fixture.ModeOK, "b")
	res2, err := Run(context.Background(), cfg2)
	if err != nil {
		t.Fatal(err)
	}
	if res1.SHA256 != res2.SHA256 {
		t.Fatalf("rerun checksum differs: %s vs %s", res1.SHA256, res2.SHA256)
	}
}

func TestConcurrencyIndependentOrder(t *testing.T) {
	cfg1 := baseCfg(t, fixture.ModeOK, "w1")
	cfg1.Workers, cfg1.QueueCap = 1, 2
	cfg4 := baseCfg(t, fixture.ModeOK, "w4")
	cfg4.Workers, cfg4.QueueCap = 4, 16
	r1, err := Run(context.Background(), cfg1)
	if err != nil {
		t.Fatal(err)
	}
	r4, err := Run(context.Background(), cfg4)
	if err != nil {
		t.Fatal(err)
	}
	if !r1.OrderedEqual || !r4.OrderedEqual || r1.SHA256 != r4.SHA256 {
		t.Fatalf("workers=1 and workers=4 diverged: %+v %+v", r1, r4)
	}
}

// Cross-subsystem: demux of a corrupt input through to fault localization.
func TestCorruptLocalizationWithinTolerance(t *testing.T) {
	cfg := baseCfg(t, fixture.ModeCorruptSample, "corrupt")
	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "ok" {
		t.Fatalf("recoverable corruption must still complete: %+v", res)
	}
	if res.FirstErrorOffset < 0 {
		t.Fatal("first error offset must be reported")
	}
	if res.OffsetErrorBytes > 16 {
		t.Fatalf("localization error %d exceeds 16 bytes", res.OffsetErrorBytes)
	}
}

// Cross-subsystem: truncated tail stays recoverable and still produces output.
func TestTruncatedTailRecoverable(t *testing.T) {
	cfg := baseCfg(t, fixture.ModeTruncatedTail, "tail")
	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("tail truncation must be recoverable: %v", err)
	}
	if res.Status != "ok" || res.Samples == 0 {
		t.Fatalf("expected partial recovery, got %+v", res)
	}
}

// Structural input must surface as an unrecoverable error.
func TestOutOfRangeStructural(t *testing.T) {
	cfg := baseCfg(t, fixture.ModeIndexOutOfRange, "oor")
	if _, err := Run(context.Background(), cfg); err == nil {
		t.Fatal("out-of-range index must be a structural error")
	}
}

// Cross-subsystem: cancellation propagates through a full queue to merge.
func TestCancelWithFullQueue(t *testing.T) {
	cfg := baseCfg(t, fixture.ModeOK, "cancel")
	cfg.Workers, cfg.QueueCap = 2, 1
	// A never-closing gate keeps every worker blocked so the queue fills and
	// the producer is parked in Submit when cancellation arrives.
	gate := make(chan struct{})
	cfg.WorkerGate = gate
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	res, err := Run(ctx, cfg)
	if err != nil {
		t.Fatalf("cancellation is expected, not an error: %v", err)
	}
	if res.Status != "cancelled" {
		t.Fatalf("want cancelled, got %+v", res)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("pipeline did not exit within 3 seconds after cancel")
	}
}

func TestResumeIsFast(t *testing.T) {
	cfg := baseCfg(t, fixture.ModeOK, "resume")
	if _, err := Run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.ResumeMS > 500 {
		t.Fatalf("resume took %d ms, target <500", res.ResumeMS)
	}
}

func TestBothVariants(t *testing.T) {
	for variant := 0; variant < 2; variant++ {
		cfg := baseCfg(t, fixture.ModeOK, "variant")
		cfg.Variant = variant
		res, err := Run(context.Background(), cfg)
		if err != nil {
			t.Fatalf("variant %d: %v", variant, err)
		}
		if res.Tracks < 3 || !res.OrderedEqual {
			t.Fatalf("variant %d bad result: %+v", variant, res)
		}
	}
}
