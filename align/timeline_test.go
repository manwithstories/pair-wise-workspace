package align_test

import (
	"testing"
	"time"

	"github.com/fashionweek/mediad/internal/align"
	"github.com/fashionweek/mediad/internal/clock"
	"github.com/fashionweek/mediad/internal/media"
	"github.com/fashionweek/mediad/internal/mp4"
)

// synthTrack builds a video track of n samples with the given (possibly
// fractional) frame duration, marking every kfEvery sample as a sync point.
// Offsets/sizes are valid-looking but irrelevant to alignment.
func synthTrack(t *testing.T, fpsNum, fpsDen int64, n int, kfEvery int) *mp4.TrackIndex {
	t.Helper()
	frame, err := clock.New(fpsDen, fpsNum) // seconds per frame
	if err != nil {
		t.Fatal(err)
	}
	ts := uint32(90000)
	den := int64(ts)
	samples := make([]mp4.Sample, n)
	var pos clock.Rat
	zero, _ := clock.New(0, 1)
	pos = zero
	for i := 0; i < n; i++ {
		// Convert the exact rational position into timescale ticks.
		ticks := pos.Num * den / pos.Den
		dts, err := clock.New(ticks, den)
		if err != nil {
			t.Fatal(err)
		}
		samples[i] = mp4.Sample{
			Index: i, Offset: 1000 + int64(i)*100, Size: 100,
			DTS: dts, PTS: dts, Duration: frame, Sync: i%kfEvery == 0,
		}
		next, err := pos.Add(frame)
		if err != nil {
			t.Fatal(err)
		}
		pos = next
	}
	return &mp4.TrackIndex{
		TrackID: 1, Video: true, Codec: "avc1",
		Timescale: ts, Samples: samples,
	}
}

func cam(name string, creation time.Time, track *mp4.TrackIndex) *align.Camera {
	return &align.Camera{Name: name, Path: name + ".mp4", Creation: creation, Track: track}
}

func TestAlignsTwoCamerasWithDifferentFrameRates(t *testing.T) {
	base := time.Date(2026, 11, 28, 20, 0, 0, 0, time.UTC)
	// Both cameras start at the same wall instant but run different rates:
	// A at 30 fps, B at the NTSC-style 30000/1001. Keyframes every 2s (60 and
	// ~60 frames) so cut points land on an IDR of each camera.
	a := cam("A", base, synthTrack(t, 30, 1, 240, 60))
	b := cam("B", base, synthTrack(t, 30000, 1001, 240, 60))

	tl, err := align.Build([]*align.Camera{a, b})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	seg, err := clock.New(2, 1)
	if err != nil {
		t.Fatal(err)
	}
	cuts, err := tl.Plan(seg)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(cuts) == 0 {
		t.Fatal("expected at least one cut")
	}
	// Every cut must carry a range for both cameras and hit a sync sample.
	for _, c := range cuts {
		if len(c.Ranges) != 2 {
			t.Fatalf("cut %d missing camera ranges: %+v", c.Index, c.Ranges)
		}
		for name, r := range c.Ranges {
			idx := indexOf(tl.Frames[name], r.Keyframe)
			if idx < 0 || !tl.Frames[name][idx].Sync {
				t.Fatalf("cut %d camera %s keyframe not a sync sample", c.Index, name)
			}
		}
	}
}

func indexOf(frames []align.Frame, sample int) int {
	for i, f := range frames {
		if f.Sample == sample {
			return i
		}
	}
	return -1
}

func TestRejectsDeviationBeyondOneFrame(t *testing.T) {
	base := time.Date(2026, 11, 28, 20, 0, 0, 0, time.UTC)
	// Two 30fps cameras whose origins differ by half a second: no keyframe of
	// the late camera can reach the nominal cut within one frame (1/30s).
	a := cam("A", base, synthTrack(t, 30, 1, 240, 30))
	b := cam("B", base.Add(500*time.Millisecond), synthTrack(t, 30, 1, 240, 30))

	tl, err := align.Build([]*align.Camera{a, b})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	seg, _ := clock.New(2, 1)
	_, err = tl.Plan(seg)
	if err == nil {
		t.Fatal("expected cross-camera deviation rejection")
	}
	me, ok := media.As(err)
	if !ok || me.Phase != media.PhaseAlign {
		t.Fatalf("want structured align error, got %v", err)
	}
	if !me.HasTime {
		t.Fatal("deviation error must retain the media time")
	}
}

func TestVFRHandledExactly(t *testing.T) {
	base := time.Date(2026, 11, 28, 20, 0, 0, 0, time.UTC)
	tr := synthTrack(t, 30, 1, 90, 30)
	// Perturb every third frame duration to simulate VFR (exact rationals).
	for i := range tr.Samples {
		if i%3 == 0 {
			d, _ := clock.New(1, 20) // 50ms instead of ~33ms
			tr.Samples[i].Duration = d
		}
	}
	c := cam("V", base, tr)
	tl, err := align.Build([]*align.Camera{c})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	// The timeline tail equals the final frame's PTS plus its own duration,
	// using exact rational arithmetic throughout (no float frame counting).
	last := tr.Samples[len(tr.Samples)-1]
	want, err := last.PTS.Add(last.Duration)
	if err != nil {
		t.Fatal(err)
	}
	if !tl.End.Equal(want) {
		t.Fatalf("VFR tail %s != exact end %s", tl.End, want)
	}
}

func TestNonOverlappingCamerasRejected(t *testing.T) {
	base := time.Date(2026, 11, 28, 20, 0, 0, 0, time.UTC)
	// 2s of footage each, origins 10s apart => no overlap.
	a := cam("A", base, synthTrack(t, 30, 1, 60, 30))
	b := cam("B", base.Add(10*time.Second), synthTrack(t, 30, 1, 60, 30))
	if _, err := align.Build([]*align.Camera{a, b}); err == nil {
		t.Fatal("expected non-overlap rejection")
	}
}
