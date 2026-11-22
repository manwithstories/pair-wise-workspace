package audio

import (
	"testing"

	"mediacore/internal/clock"
)

func tone(n int, amp int16) []int16 {
	s := make([]int16, n)
	for i := range s {
		if i%2 == 0 {
			s[i] = amp
		} else {
			s[i] = -amp
		}
	}
	return s
}

func TestDetectsSilenceRun(t *testing.T) {
	d, err := NewDetector(1000, Params{WindowSamples: 100, Threshold: 10})
	if err != nil {
		t.Fatal(err)
	}
	// 1s tone, 2s silence, 1s tone at 1000 Hz, window 0.1s.
	d.Feed(tone(1000, 500))
	d.Feed(make([]int16, 2000))
	d.Feed(tone(1000, 500))
	segs, err := d.Finish()
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 1 {
		t.Fatalf("got %d segments, want 1", len(segs))
	}
	if segs[0].Start != clock.Must(1, 1) || segs[0].End != clock.Must(3, 1) {
		t.Fatalf("silence = [%v,%v), want [1,3)", segs[0].Start, segs[0].End)
	}
}

func TestShortTailNotReported(t *testing.T) {
	d, _ := NewDetector(1000, Params{WindowSamples: 100, Threshold: 10})
	d.Feed(tone(250, 500))    // 0.25s tone
	d.Feed(make([]int16, 50)) // 0.05s silence, shorter than the window
	segs, err := d.Finish()
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 0 {
		t.Fatalf("tail shorter than window must not be reported: %+v", segs)
	}
}

func TestWholeInputShorterThanWindow(t *testing.T) {
	d, _ := NewDetector(1000, Params{WindowSamples: 100, Threshold: 10})
	d.Feed(make([]int16, 99))
	segs, _ := d.Finish()
	if len(segs) != 0 {
		t.Fatalf("got %+v, want no segments", segs)
	}
}

func TestThresholdBoundary(t *testing.T) {
	d, _ := NewDetector(1000, Params{WindowSamples: 100, Threshold: 10})
	d.Feed(tone(100, 10)) // peak == threshold -> silent
	segs, _ := d.Finish()
	if len(segs) != 1 {
		t.Fatalf("peak at threshold should be silence: %+v", segs)
	}
	d2, _ := NewDetector(1000, Params{WindowSamples: 100, Threshold: 10})
	d2.Feed(tone(100, 11))
	segs2, _ := d2.Finish()
	if len(segs2) != 0 {
		t.Fatalf("peak above threshold should not be silence: %+v", segs2)
	}
}

func TestAdjacentRunsMerge(t *testing.T) {
	d, _ := NewDetector(1000, Params{WindowSamples: 100, Threshold: 10})
	d.Feed(make([]int16, 300)) // 3 silent windows
	segs, _ := d.Finish()
	if len(segs) != 1 || segs[0].End != clock.Must(3, 10) {
		t.Fatalf("adjacent windows should merge: %+v", segs)
	}
}

func TestInvalidParams(t *testing.T) {
	if _, err := NewDetector(0, Params{WindowSamples: 100}); err == nil {
		t.Fatal("zero rate should fail")
	}
	if _, err := NewDetector(1000, Params{WindowSamples: 0}); err == nil {
		t.Fatal("zero window should fail")
	}
}
