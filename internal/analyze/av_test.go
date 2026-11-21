package analyze

import (
	"testing"

	"mediascan/internal/model"
)

func track(timescale uint32, base int64, step int64, n int) Track {
	var t Track
	t.Timescale = timescale
	for i := 0; i < n; i++ {
		t.Samples = append(t.Samples, SyncSample{PTS: base + int64(i)*step, Segment: "seg", Sync: i%3 == 0})
	}
	return t
}

func TestAlignReportsConstantClockOffset(t *testing.T) {
	// 30 fps video and 48 kHz audio, audio shifted 10 ms late.
	video := track(90000, 0, 3000, 90)
	audio := track(48000, 480, 1024, 300)
	res, err := Align(video, audio, 40)
	if err != nil {
		t.Fatal(err)
	}
	if res.MaxErrorMS != 10 {
		t.Fatalf("max error %dms, want 10", res.MaxErrorMS)
	}
	if res.ClockSkews == 0 {
		t.Fatalf("constant offset should be a clock skew: %+v", res)
	}
	if res.RealJumps != 0 {
		t.Fatalf("nothing should be a real jump: %+v", res)
	}
	if res.MaxErrorSeg == "" {
		t.Fatal("max error segment not attributed")
	}
}

func TestAlignFlagsSpikeAsRealJump(t *testing.T) {
	video := track(90000, 0, 3000, 60)
	audio := track(48000, 0, 1024, 200)
	// A single isolated +30 ms audio displacement mid-window.
	audio.Samples[50].PTS += 1440
	res, err := Align(video, audio, 5)
	if err != nil {
		t.Fatal(err)
	}
	if res.MaxErrorMS < 20 {
		t.Fatalf("spike not detected: %+v", res)
	}
	if res.RealJumps == 0 {
		t.Fatalf("spike should be classified as a real jump: %+v", res)
	}
}

func TestAlignWithinToleranceYieldsNoFaults(t *testing.T) {
	video := track(90000, 0, 3000, 60)
	audio := track(48000, 0, 1024, 200)
	res, err := Align(video, audio, 40)
	if err != nil {
		t.Fatal(err)
	}
	if res.MaxErrorMS > 40 {
		t.Fatalf("unexpected skew: %+v", res)
	}
	if len(res.Faults) != 0 {
		t.Fatalf("faults within tolerance: %+v", res.Faults)
	}
}

func TestAlignDetectsWrap(t *testing.T) {
	video := track(90000, 0, 3000, 10)
	video.Samples = append(video.Samples, SyncSample{PTS: 0, Segment: "wrap"})
	audio := track(48000, 0, 1024, 10)
	res, err := Align(video, audio, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if res.WrapSpans != 1 {
		t.Fatalf("wrap spans %d", res.WrapSpans)
	}
	if len(res.Faults) == 0 || res.Faults[0].Kind != model.FaultTimestamp {
		t.Fatalf("wrap not reported: %+v", res.Faults)
	}
}

func TestKeyframesDetectsLongGOP(t *testing.T) {
	video := track(90000, 0, 3000, 120)
	video.Samples = video.Samples[:0]
	for i := 0; i < 120; i++ {
		video.Samples = append(video.Samples, SyncSample{PTS: int64(i) * 3000, Segment: "seg", Sync: i == 0 || i == 90})
	}
	rep := Keyframes(video, 2000)
	if rep.MaxIntervalMS != 3000 {
		t.Fatalf("max GOP %d", rep.MaxIntervalMS)
	}
	if rep.Irregular != 1 || len(rep.Faults) != 1 {
		t.Fatalf("irregular=%d faults=%+v", rep.Irregular, rep.Faults)
	}
	if rep.Faults[0].Kind != model.FaultKeyframe {
		t.Fatalf("fault kind %s", rep.Faults[0].Kind)
	}
}

func TestAlignOverflowGuard(t *testing.T) {
	video := track(1, 1<<62, 1, 4)
	audio := track(1, 0, 1, 4)
	if _, err := Align(video, audio, 10); err == nil {
		t.Fatal("expected overflow when scaling huge timestamps")
	}
}
