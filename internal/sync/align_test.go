package sync

import (
	"errors"
	"testing"

	"relayctl/internal/clock"
)

func pts(starts ...int64) []clock.SamplePoint {
	out := make([]clock.SamplePoint, len(starts))
	for i, s := range starts {
		out[i] = clock.SamplePoint{Index: i, StartMS: s, DurationMS: 20}
	}
	return out
}

func TestAlignInSync(t *testing.T) {
	mc := clock.New()
	a := New(mc)
	audio := TrackTimeline{TrackID: 1, Kind: "audio", Points: pts(0, 20, 40)}
	video := TrackTimeline{TrackID: 2, Kind: "video", Points: pts(0, 20, 40)}
	res, err := a.Align(audio, video)
	if err != nil || res.Blocked || res.WorstAbsMS != 0 {
		t.Fatalf("aligned tracks: %+v err=%v", res, err)
	}
	if res.Confidence != Confirmed {
		t.Fatalf("confidence = %v want confirmed", res.Confidence)
	}
}

func TestAlignSmallOffsetUncertain(t *testing.T) {
	mc := clock.New()
	a := New(mc)
	// Audio 40 ms ahead: inside the 125 ms threshold but not perfect.
	audio := TrackTimeline{TrackID: 1, Points: pts(40, 60, 80)}
	video := TrackTimeline{TrackID: 2, Points: pts(0, 20, 40)}
	res, err := a.Align(audio, video)
	if err != nil || res.Blocked {
		t.Fatalf("in-threshold offset must pass: %v", err)
	}
	if res.WorstAbsMS != 40 || res.Confidence != Uncertain {
		t.Fatalf("res = %+v", res)
	}
	if res.Offsets[0].DeltaMS != 40 {
		t.Fatalf("signed delta = %d want 40", res.Offsets[0].DeltaMS)
	}
}

func TestAlignExcessiveOffsetBlocks(t *testing.T) {
	mc := clock.New()
	a := New(mc)
	audio := TrackTimeline{TrackID: 1, Points: pts(200, 220)}
	video := TrackTimeline{TrackID: 2, Points: pts(0, 20)}
	res, err := a.Align(audio, video)
	if !errors.Is(err, ErrTimelineBlocked) || !res.Blocked {
		t.Fatalf("excessive offset must block: res=%+v err=%v", res, err)
	}
	if res.Confidence != Rejected {
		t.Fatalf("confidence = %v want rejected", res.Confidence)
	}
}

func TestAlignClockRollbackBlocks(t *testing.T) {
	mc := clock.New()
	_ = mc.AddTrack(clock.TrackConfig{TrackID: 1, Timescale: 48000})
	_ = mc.AddTrack(clock.TrackConfig{TrackID: 2, Timescale: 90000})
	// Force a tfdt rollback on the audio track through the shared clock.
	if _, err := mc.ObserveTfdt(1, 100000); err != nil {
		t.Fatal(err)
	}
	if _, err := mc.ObserveTfdt(1, 90000); err == nil {
		t.Fatal("expected rollback error")
	}
	a := New(mc)
	audio := TrackTimeline{TrackID: 1, Points: pts(0, 20)}
	video := TrackTimeline{TrackID: 2, Points: pts(0, 20)}
	res, err := a.Align(audio, video)
	if !errors.Is(err, ErrTimelineBlocked) {
		t.Fatalf("clock rollback must block packaging: %v", err)
	}
	if res.BlockingKind != clock.ErrTfdtRollback {
		t.Fatalf("blocking kind = %q want tfdt_rollback", res.BlockingKind)
	}
}

func TestAlignThresholdOverride(t *testing.T) {
	mc := clock.New()
	a := New(mc).WithThreshold(10)
	audio := TrackTimeline{TrackID: 1, Points: pts(20)}
	video := TrackTimeline{TrackID: 2, Points: pts(0)}
	if _, err := a.Align(audio, video); !errors.Is(err, ErrTimelineBlocked) {
		t.Fatal("20ms offset under a 10ms threshold must block")
	}
}

func TestAlignUnevenLengths(t *testing.T) {
	mc := clock.New()
	a := New(mc)
	audio := TrackTimeline{TrackID: 1, Points: pts(0, 20)}
	video := TrackTimeline{TrackID: 2, Points: pts(0, 20, 40, 60)}
	res, err := a.Align(audio, video)
	if err != nil || len(res.Offsets) != 2 {
		t.Fatalf("align to shorter track: %+v err=%v", res, err)
	}
}
