package report

import (
	"bytes"
	"math"
	"strings"
	"testing"
)

func TestPSNR(t *testing.T) {
	// MSE chosen so PSNR is exactly 38.2 dB.
	mse := 255.0 * 255.0 / math.Pow(10, 3.82)
	got := PSNR(mse*1000, 1000)
	if got < 38.19 || got > 38.21 {
		t.Fatalf("psnr %v want 38.2", got)
	}
	if PSNR(0, 100) != 99.0 {
		t.Fatal("zero error should saturate")
	}
}

func TestDeltaPercent(t *testing.T) {
	if got := DeltaPercent(2280, 2400); got != -5.0 {
		t.Fatalf("delta %v want -5", got)
	}
	if got := DeltaPercent(0, 0); got != 0 {
		t.Fatalf("zero target delta %v", got)
	}
}

func TestRender(t *testing.T) {
	m := Metrics{
		Fixture: "generated", Segments: 12, Repaired: 1, Unrecoverable: 1,
		IndexEntries: 10, TargetKbps: 2400, ActualKbps: 2280,
		DeltaPct: -5.0, PSNRdB: 38.2, ManifestErrMs: 11.1,
		PeakMemBytes: 2 << 20, BudgetMiB: 32,
	}
	var buf bytes.Buffer
	m.Render(&buf)
	out := buf.String()
	for _, want := range []string{
		"fixture=generated", "segments=12", "repaired=1", "unrecoverable=1",
		"index_entries=10", "target_bitrate_kbps=2400", "actual_bitrate_kbps=2280",
		"bitrate_delta_pct=-5.00", "quality_psnr_db=38.2", "manifest_error_ms=11.1",
		"peak_memory_mib=2", "budget_mib=32",
	} {
		if !strings.Contains(out, want+"\n") {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}
