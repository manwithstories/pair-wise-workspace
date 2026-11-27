package audit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"avcore/internal/clock"
	"avcore/internal/state"
)

func segRec(cut int, bytes int64, bps int, psnr, ssim float64) state.SegmentRecord {
	return state.SegmentRecord{
		CutIndex: cut, Path: "x.m4s", Bytes: bytes, ActualBPS: bps,
		PSNRMeanDB: psnr, SSIMMean: ssim,
		StartTicks: int64(cut) * clock.TicksPerSecond,
		EndTicks:   int64(cut+1) * clock.TicksPerSecond,
	}
}

func TestComputeVariant(t *testing.T) {
	// Two 1-second segments, 200000 bytes each => 1.6 Mbps actual per second.
	recs := []state.SegmentRecord{
		segRec(0, 200000, 1, 30.0, 0.95),
		segRec(1, 200000, 1, 32.0, 0.97),
	}
	m := ComputeVariant("low", 1_600_000, recs)
	if m.SegmentCount != 2 || m.ActualBPS != 1_600_000 {
		t.Errorf("actual bps = %d, want 1600000", m.ActualBPS)
	}
	if m.BitrateDeltaPct != 0 {
		t.Errorf("delta = %f, want 0", m.BitrateDeltaPct)
	}
	if m.PSNRMeanDB != 31.0 {
		t.Errorf("psnr mean = %f, want 31.0", m.PSNRMeanDB)
	}
	if m.SSIMMean != 0.96 {
		t.Errorf("ssim mean = %f, want 0.96", m.SSIMMean)
	}
	if m.StartTicks != 0 || m.EndTicks != 2*clock.TicksPerSecond {
		t.Errorf("span wrong: %d..%d", m.StartTicks, m.EndTicks)
	}
}

func TestBitrateDeltaSignAndClamp(t *testing.T) {
	recs := []state.SegmentRecord{segRec(0, 115000, 1, 40, 0.99)} // 0.92 Mbps
	m := ComputeVariant("v", 1_000_000, recs)
	// 920000 vs 1000000 => -8%
	if m.BitrateDeltaPct < -8.1 || m.BitrateDeltaPct > -7.9 {
		t.Errorf("delta = %f, want about -8", m.BitrateDeltaPct)
	}
}

func TestComputeVariantEmpty(t *testing.T) {
	m := ComputeVariant("none", 1000, nil)
	if m.SegmentCount != 0 || m.ActualBPS != 0 {
		t.Errorf("empty variant must zero out: %+v", m)
	}
}

func TestAlignmentMS(t *testing.T) {
	a := []int64{0, 90000, 180000}
	b := []int64{0, 90000, 180000}
	if got := AlignmentMS([][]int64{a, b}); got != 0 {
		t.Errorf("identical plans align = %f, want 0", got)
	}
	// One tick of 90 kHz = 1/90 ms. Introduce a 900-tick skew = 10 ms.
	c := []int64{0, 90900, 180000}
	if got := AlignmentMS([][]int64{a, c}); got != 10.0 {
		t.Errorf("skewed align = %f, want 10.0", got)
	}
	if got := AlignmentMS([][]int64{a}); got != 0 {
		t.Errorf("single variant align = %f, want 0", got)
	}
}

func TestLoggerEmitsJSONL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	l, err := NewLogger(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Emit(Record{"event": "segment_committed", "cut": 0, "bps": 900000}); err != nil {
		t.Fatal(err)
	}
	if err := l.Emit(Record{"event": "manifest_written", "sha": "abc"}); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 {
		t.Fatalf("JSONL lines = %d, want 2", len(lines))
	}
	var first map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("first line not valid JSON: %v", err)
	}
	if first["event"] != "segment_committed" || first["cut"].(float64) != 0 {
		t.Errorf("unexpected payload: %+v", first)
	}
	if _, ok := first["ts"]; !ok {
		t.Error("auto timestamp missing")
	}
}

func TestRound(t *testing.T) {
	if round(31.45678, 2) != 31.46 {
		t.Errorf("round positive = %v", round(31.45678, 2))
	}
	if round(-8.1234, 1) != -8.1 {
		t.Errorf("round negative = %v", round(-8.1234, 1))
	}
}
