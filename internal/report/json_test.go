package report

import (
	"bytes"
	"math"
	"strings"
	"testing"

	"mediad/internal/fault"
	"mediad/internal/pipeline"
	"mediad/internal/timeline"
)

func sampleResult() *pipeline.Result {
	return &pipeline.Result{
		Frames: 2,
		FrameStats: []pipeline.FrameStat{
			{Track: "v1", Seq: 0, Bytes: 1000, MSE: 10, PSNR: 38.0},
			{Track: "v1", Seq: 1, Bytes: 1000, MSE: 20, PSNR: 36.0},
		},
		ProxyBytes:  2000,
		DurationSec: 0.5,
		AlignMaxMs:  0.5,
		CorruptRanges: []timeline.HalfOpen{
			{Start: 1.0, End: 1.5},
		},
		RecoveredFrames: 1,
		AudioSamples:    map[string]int64{"a1": 48000},
		Faults: []fault.Fault{
			{Track: "v1", Frame: 2, Kind: fault.KindTruncated, Recoverable: true},
		},
	}
}

func TestFromResultAndLine(t *testing.T) {
	c := FromResult("corrupt", 32000, sampleResult(), 0)
	if c.ActualBPS != 32000 || c.DeviationPct != 0 {
		t.Fatalf("bitrate: %+v", c)
	}
	if c.PSNRP50DB != 37.0 || c.MSE != 15.0 {
		t.Fatalf("quality: %+v", c)
	}
	if c.CorruptRanges != 1 || c.RecoveredFrames != 1 {
		t.Fatalf("ranges: %+v", c)
	}
	line := c.Line()
	for _, want := range []string{"case=corrupt", "target_bps=32000", "frames=2",
		"corrupt_ranges=1", "recovered_frames=1", "align_max_ms=0.5"} {
		if !strings.Contains(line, want) {
			t.Fatalf("line missing %q: %s", want, line)
		}
	}
}

func TestDeterministicWrite(t *testing.T) {
	r := &Report{Version: 1, Cases: []CaseReport{FromResult("a", 1000, sampleResult(), 0)}}
	var b1, b2 bytes.Buffer
	if err := Write(&b1, r); err != nil {
		t.Fatal(err)
	}
	if err := Write(&b2, r); err != nil {
		t.Fatal(err)
	}
	if b1.String() != b2.String() {
		t.Fatal("non-deterministic report")
	}
	if !strings.HasSuffix(b1.String(), "\n") {
		t.Fatal("missing trailing newline")
	}
}

func TestInfClamped(t *testing.T) {
	if got := round4(math.Inf(1)); got != 99.99 {
		t.Fatalf("inf clamp: %v", got)
	}
	if got := round4(math.NaN()); got != 0 {
		t.Fatalf("nan clamp: %v", got)
	}
}
