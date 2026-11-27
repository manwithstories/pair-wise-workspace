package analysis

import "testing"

func TestQuantile(t *testing.T) {
	series := []float32{0.2, 0.0, 0.1, 0.3, 0.4}
	if got := quantile(series, 0.2); got != 0.1 {
		t.Fatalf("q0.2 = %v, want 0.1", got)
	}
	if got := quantile(series, 0.0); got != 0.0 {
		t.Fatalf("q0 = %v, want 0", got)
	}
	// Input ordering must not be mutated.
	if series[0] != 0.2 {
		t.Fatal("quantile mutated the input series")
	}
}

func TestClassifyChannelGateDecisions(t *testing.T) {
	// A steady, loud tone: every frame well above the absolute floor.
	loud := make([]frameLevel, 10)
	for i := range loud {
		loud[i] = frameLevel{rms: []float32{0.25}, start90k: int64(i) * 90000, end90k: int64(i+1) * 90000}
	}
	cs := classifyChannel(0, loud, DefaultSilenceConfig())
	if len(cs.Intervals) != 0 {
		t.Fatalf("steady tone produced silence intervals: %+v", cs.Intervals)
	}

	// Genuine silence: all frames at zero must yield an interval.
	silent := make([]frameLevel, 10)
	for i := range silent {
		silent[i] = frameLevel{rms: []float32{0.0}, start90k: int64(i) * 90000, end90k: int64(i+1) * 90000}
	}
	cs = classifyChannel(0, silent, DefaultSilenceConfig())
	if len(cs.Intervals) != 1 {
		t.Fatalf("silence produced %d intervals, want 1", len(cs.Intervals))
	}
}
