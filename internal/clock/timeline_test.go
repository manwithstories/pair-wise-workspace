package clock

import (
	"errors"
	"math"
	"testing"

	"avpipe/internal/fault"
)

func TestSamplesToTicksExact(t *testing.T) {
	cases := []struct {
		samples int64
		rate    int
		want    Tick
	}{
		{0, 48000, 0},
		{480, 48000, 900},
		{1, 90000, 1},
		{90000, 90000, 90000},
		{48000, 48000, 90000},
		{1, 48000, 1}, // truncation by design; caller keeps the remainder
	}
	for _, c := range cases {
		got, _ := SamplesToTicks(c.samples, c.rate)
		if got != c.want {
			t.Fatalf("SamplesToTicks(%d,%d)=%d want %d", c.samples, c.rate, got, c.want)
		}
	}
}

func TestRescaleDeterministicAndExact(t *testing.T) {
	for i := 0; i < 5000; i++ {
		v := int64(i * 37)
		out, rem := Rescale(v, 44100, 90000)
		if out*44100+rem != v*90000 {
			t.Fatalf("Rescale lost value at %d", v)
		}
	}
}

func TestSamplesForDurationRoundsNearest(t *testing.T) {
	if got := SamplesForDuration(900, 48000); got != 480 {
		t.Fatalf("1ms at 48k = %d want 480", got)
	}
	if got := SamplesForDuration(Tick(0.5*float64(Timescale)/1000), 48000); got != 24 {
		t.Fatalf("0.5ms at 48k = %d want 24", got)
	}
}

func TestCheckMonotonic(t *testing.T) {
	if e := CheckMonotonic([]Span{{0, 10}, {10, 20}}); e != nil {
		t.Fatalf("adjacent spans flagged: %+v", e)
	}
	err := CheckMonotonic([]Span{{0, 10}, {10, 20}, {15, 25}})
	if err == nil || err.Index != 2 {
		t.Fatalf("overlap not detected: %+v", err)
	}
	if e := CheckMonotonic([]Span{{5, 4}}); e == nil {
		t.Fatal("inverted span accepted")
	}
}

func TestPercentileNearestRank(t *testing.T) {
	vals := []float64{5, 1, 4, 2, 3}
	if got := Percentile(vals, 0.95); got != 5 {
		t.Fatalf("p95=%v want 5", got)
	}
	if got := Percentile(vals, 0.5); got != 3 {
		t.Fatalf("p50=%v want 3", got)
	}
	if vals[0] != 5 {
		t.Fatal("Percentile mutated input")
	}
	if got := Percentile(nil, 0.9); got != 0 {
		t.Fatalf("empty percentile=%v", got)
	}
}

func TestFromMillisRoundTrip(t *testing.T) {
	for _, ms := range []float64{0, 0.5, 1, 35, 120, 1000} {
		tk := FromMillis(ms)
		if math.Abs(tk.Millis()-ms) > 1e-9 {
			t.Fatalf("round trip %v -> %v", ms, tk.Millis())
		}
	}
}

func TestExitCodes(t *testing.T) {
	cases := map[fault.Code]int{
		fault.CodeInvalidInput:      2,
		fault.CodeWAVTruncated:      2,
		fault.CodeUnsupportedLayout: 2,
		fault.CodeMetric:            3,
		fault.CodeInternal:          4,
		fault.CodeRTP:               4,
	}
	for code, want := range cases {
		err := fault.New(code, "test", "boom")
		if got := fault.ExitCode(err); got != want {
			t.Fatalf("%s exit=%d want %d", code, got, want)
		}
	}
	inner := fault.New(fault.CodeWAVLength, "audio.wav.parse", "bad length").At(17)
	if got := fault.Root(inner); got.Code != fault.CodeWAVLength || got.Offset != 17 {
		t.Fatalf("root lost location: %+v", got)
	}
	if got := fault.ExitCode(nil); got != 0 {
		t.Fatalf("nil exit=%d", got)
	}
	if got := fault.FirstRoot(nil, inner, errors.New("later")); got.Code != fault.CodeWAVLength {
		t.Fatalf("FirstRoot=%+v", got)
	}
}
