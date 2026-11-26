package sync

import (
	"testing"

	"mediaqc/internal/media"
)

func alignedFrames(n int) ([]Frame, []Frame) {
	audio := make([]Frame, n)
	video := make([]Frame, n)
	for i := 0; i < n; i++ {
		audio[i] = Frame{Stream: "audio", Index: i, PTS: uint64(i) * 1800, DurationTicks: 1800}
		video[i] = Frame{Stream: "video", Index: i, PTS: uint64(i) * 1800, DurationTicks: 1800}
	}
	return audio, video
}

func TestAlignBaselineOnSharedClock(t *testing.T) {
	audio, video := alignedFrames(48)
	rep := Align(audio, video)
	if rep.P95AbsMS != 0 {
		t.Fatalf("perfect alignment p95=%.3f", rep.P95AbsMS)
	}
	if rep.PropagatedGaps != 0 {
		t.Fatalf("unexpected gaps: %d", rep.PropagatedGaps)
	}
}

func TestBurstGapPropagatesToVideo(t *testing.T) {
	audio, video := alignedFrames(48)
	audio[10].Gap = true
	audio[11].Gap = true

	rep := Align(audio, video)
	if rep.PropagatedGaps != 2 {
		t.Fatalf("propagated=%d want 2", rep.PropagatedGaps)
	}
	if rep.MaxGapOffsetMS < 19.99 || rep.MaxGapOffsetMS > 20.01 {
		t.Fatalf("gap offset=%.2fms want 20ms", rep.MaxGapOffsetMS)
	}
	if rep.P95AbsMS > 20 {
		t.Fatalf("p95=%.2fms exceeds target", rep.P95AbsMS)
	}
	var found bool
	for _, s := range rep.Samples {
		if s.GapPropagated {
			found = true
			if s.AbsMS < 19.99 {
				t.Fatalf("gap sample offset too small: %+v", s)
			}
		}
	}
	if !found {
		t.Fatal("no propagated sample recorded")
	}
}

func TestAACClockTicksRemainBounded(t *testing.T) {
	desc, err := media.AACFrameDuration(48_000, 1)
	if err != nil {
		t.Fatal(err)
	}
	audio := make([]Frame, 48)
	video := make([]Frame, 48)
	for i := range audio {
		audio[i] = Frame{PTS: uint64(i) * desc.ClockTicks, DurationTicks: desc.ClockTicks}
		// 50 fps video on the same 90 kHz clock.
		video[i] = Frame{PTS: uint64(i) * 1800, DurationTicks: 1800}
	}
	rep := Align(audio, video)
	if rep.P95AbsMS > 20 {
		t.Fatalf("AAC drift p95=%.2f >20", rep.P95AbsMS)
	}
}

func TestPercentileNearestRank(t *testing.T) {
	if got := Percentile([]float64{1, 2, 3, 4}, 0.95); got != 4 {
		t.Fatalf("p95=%v want 4", got)
	}
	if got := Percentile(nil, 0.95); got != 0 {
		t.Fatalf("empty p95=%v", got)
	}
}
