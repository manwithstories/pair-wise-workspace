package audio

import (
	"testing"
)

func newTestDetector(t *testing.T) *Detector {
	t.Helper()
	// 1000 Hz rate, 100 ms windows -> 100 samples each; min silence 300 ms.
	det, err := NewDetector(SilenceConfig{
		SampleRate:       1000,
		WindowMillis:     100,
		ThresholdRMS:     0.01,
		MinSilenceMillis: 300,
	})
	if err != nil {
		t.Fatal(err)
	}
	return det
}

func TestSilenceRanges(t *testing.T) {
	det := newTestDetector(t)
	win := det.windowSize // 100 samples
	// Loud 2 windows, silent 4, loud 1.
	loud := make([]int16, win)
	for i := range loud {
		loud[i] = 8000
	}
	silent := make([]int16, win)
	det.Push(loud)
	det.Push(loud)
	for i := 0; i < 4; i++ {
		det.Push(silent)
	}
	det.Push(loud)
	ranges := det.Flush()
	if len(ranges) != 1 {
		t.Fatalf("want 1 silence range, got %d: %+v", len(ranges), ranges)
	}
	r := ranges[0]
	// Silence starts after the two loud windows (sample 200), 4 windows.
	if r.StartSample != 200 || r.EndSample != 600 {
		t.Fatalf("range bounds wrong: %+v", r)
	}
	if r.Millis() != 400 {
		t.Fatalf("silence millis = %d, want 400", r.Millis())
	}
	if det.TotalSilenceMillis() != 400 {
		t.Fatalf("total = %d, want 400", det.TotalSilenceMillis())
	}
}

func TestPartialBoundaryWindowNoFalseSilence(t *testing.T) {
	det := newTestDetector(t)
	// Fewer samples than one full window, all zero. A partial window must
	// never be reported as silence by itself.
	det.Push(make([]int16, det.windowSize-1))
	if ranges := det.Flush(); len(ranges) != 0 {
		t.Fatalf("partial window produced false silence: %+v", ranges)
	}
}

func TestShortSilenceBelowThreshold(t *testing.T) {
	det := newTestDetector(t)
	win := det.windowSize
	loud := make([]int16, win)
	for i := range loud {
		loud[i] = 8000
	}
	// Only two silent windows = 200 ms < 300 ms minimum.
	det.Push(loud)
	det.Push(make([]int16, win))
	det.Push(make([]int16, win))
	det.Push(loud)
	if ranges := det.Flush(); len(ranges) != 0 {
		t.Fatalf("sub-threshold silence reported: %+v", ranges)
	}
}

func TestRMSThreshold(t *testing.T) {
	// A constant-amplitude window has RMS == its normalized amplitude.
	win := make([]int16, 100)
	for i := range win {
		win[i] = 328 // ~0.01 amplitude, right at boundary
	}
	got := rms(win)
	want := 328.0 / 32768.0
	if diff := got - want; diff > 1e-6 || diff < -1e-6 {
		t.Fatalf("rms = %v, want %v", got, want)
	}
}

func TestMultipleSilenceRanges(t *testing.T) {
	det := newTestDetector(t)
	win := det.windowSize
	loud := make([]int16, win)
	for i := range loud {
		loud[i] = 8000
	}
	// silent 4 windows, loud 1, silent 3 windows (300 ms, exactly minimum).
	for i := 0; i < 4; i++ {
		det.Push(make([]int16, win))
	}
	det.Push(loud)
	for i := 0; i < 3; i++ {
		det.Push(make([]int16, win))
	}
	ranges := det.Flush()
	if len(ranges) != 2 {
		t.Fatalf("want 2 ranges, got %d: %+v", len(ranges), ranges)
	}
	if ranges[0].Millis() != 400 || ranges[1].Millis() != 300 {
		t.Fatalf("durations wrong: %d %d", ranges[0].Millis(), ranges[1].Millis())
	}
	for _, r := range ranges {
		if r.Millis() < 0 {
			t.Fatalf("negative millis: %+v", r)
		}
	}
}

func TestDetectorInvalidConfig(t *testing.T) {
	if _, err := NewDetector(SilenceConfig{SampleRate: 0, WindowMillis: 20}); err == nil {
		t.Fatal("zero sample rate accepted")
	}
	if _, err := NewDetector(SilenceConfig{SampleRate: 10, WindowMillis: 1}); err == nil {
		t.Fatal("sub-sample window accepted")
	}
}
