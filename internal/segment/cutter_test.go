package segment

import (
	"testing"

	"medpipe/internal/audio"
	"medpipe/internal/clock"
	"medpipe/internal/model"
)

// twoTrackTimeline builds a 1s video track at 40ms steps and a 1s audio track
// at 20ms steps, both starting at t=0.
func twoTrackTimeline(t *testing.T) *clock.Timeline {
	t.Helper()
	tracks := []model.Track{
		{ID: "cam", Kind: model.KindVideo, Timescale: 1_000_000, ClockBits: 32, NominalTickDelta: 40_000},
		{ID: "mic", Kind: model.KindAudio, Timescale: 1_000_000, ClockBits: 32, NominalTickDelta: 20_000},
	}
	frames := map[string][]model.Frame{"cam": {}, "mic": {}}
	for i := 0; i < 25; i++ {
		frames["cam"] = append(frames["cam"], model.Frame{TrackID: "cam", Seq: uint64(i), RawTS: uint64(1000 + i*40_000), Keyframe: i%5 == 0, Payload: []byte{byte(i)}})
	}
	for i := 0; i < 50; i++ {
		frames["mic"] = append(frames["mic"], model.Frame{TrackID: "mic", Seq: uint64(i), RawTS: uint64(1000 + i*20_000), Energy: []float64{0.5}, Payload: []byte{byte(i)}})
	}
	tl, err := clock.Build(tracks, frames, clock.Options{NoDriftCorrection: true})
	if err != nil {
		t.Fatal(err)
	}
	return tl
}

func TestPlanHalfOpenWindowsAndSnapping(t *testing.T) {
	tl := twoTrackTimeline(t)
	res, err := Plan(tl, nil, Options{WindowNS: 300_000_000})
	if err != nil {
		t.Fatal(err)
	}
	segs := res.Segments
	if len(segs) != 4 {
		t.Fatalf("segments = %d, want 4", len(segs))
	}
	if segs[0].StartNS != tl.StartNS {
		t.Errorf("first segment starts at %d, want %d", segs[0].StartNS, tl.StartNS)
	}
	if segs[len(segs)-1].EndNS != tl.EndNS {
		t.Errorf("last segment ends at %d, want %d", segs[len(segs)-1].EndNS, tl.EndNS)
	}
	for i := range segs {
		if segs[i].EndNS <= segs[i].StartNS {
			t.Fatalf("empty segment %d", i)
		}
		if i > 0 && segs[i].StartNS != segs[i-1].EndNS {
			t.Fatalf("segments not contiguous at %d: %d vs %d", i, segs[i-1].EndNS, segs[i].StartNS)
		}
		if len(segs[i].Slices) != 2 {
			t.Fatalf("segment %d slices = %d, want 2", i, len(segs[i].Slices))
		}
		for _, sl := range segs[i].Slices {
			if sl.SnapErrorNS > res.MaxFrameIntervalNS {
				t.Fatalf("segment %d track %s error %d exceeds interval %d", i, sl.TrackID, sl.SnapErrorNS, res.MaxFrameIntervalNS)
			}
			if sl.StartNS != segs[i].StartNS || sl.EndNS != segs[i].EndNS {
				t.Fatalf("track slice bounds diverge from segment bounds")
			}
		}
	}
}

func TestSilenceSplitAcrossCuts(t *testing.T) {
	tl := twoTrackTimeline(t)
	silence := []audio.Range{{TrackID: "mic", StartNS: 100_000_000, EndNS: 500_000_000, Windows: 20}}
	res, err := Plan(tl, silence, Options{WindowNS: 200_000_000, SnapToleranceNS: 1})
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	var previousEnd int64
	for i, seg := range res.Segments {
		for _, ov := range seg.Silence {
			if ov.EndNS <= ov.StartNS {
				t.Fatalf("segment %d has empty overlap %+v", i, ov)
			}
			if i > 0 && ov.StartNS < previousEnd {
				t.Fatalf("silence overlap repeated across cut: %+v", ov)
			}
			total += ov.EndNS - ov.StartNS
			previousEnd = ov.EndNS
		}
	}
	if total != 400_000_000 {
		t.Fatalf("silence total across segments = %d, want 400ms", total)
	}
}

func TestPlanRejectsEmptyTimeline(t *testing.T) {
	tr := model.Track{ID: "cam", Kind: model.KindVideo, Timescale: 1000}
	tl, err := clock.Build([]model.Track{tr}, map[string][]model.Frame{"cam": nil}, clock.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Plan(tl, nil, Options{}); err == nil {
		t.Fatal("expected error for empty timeline")
	}
}

func TestSnapToleranceClampedToFrameInterval(t *testing.T) {
	tl := twoTrackTimeline(t)
	// A huge tolerance would move cuts arbitrarily; the planner must clamp it
	// to one frame interval, keeping the cross-track error bounded.
	res, err := Plan(tl, nil, Options{WindowNS: 150_000_000, SnapToleranceNS: 10_000_000_000})
	if err != nil {
		t.Fatal(err)
	}
	if res.MaxSnapErrorNS > res.MaxFrameIntervalNS {
		t.Fatalf("snap error %d exceeds interval %d", res.MaxSnapErrorNS, res.MaxFrameIntervalNS)
	}
}
