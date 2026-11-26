package sync_test

import (
	"testing"

	"medialab/internal/clock"
	"medialab/internal/model"
	"medialab/internal/sync"
)

func TestMissingAudioUsesBoundedSilence(t *testing.T) {
	audio := []sync.Frame{{Segment: 0, Present: false, DurationMS: 500, State: model.StateConcealed}}
	rep := sync.Align(nil, audio)
	if len(rep.Decisions) != 1 {
		t.Fatalf("decisions=%d", len(rep.Decisions))
	}
	if got := rep.Decisions[0].SilenceMS; got != sync.MaxSilenceMS {
		t.Fatalf("silence=%d, want bounded %d", got, sync.MaxSilenceMS)
	}
	if rep.SilenceInsertedMS != sync.MaxSilenceMS {
		t.Fatalf("total silence=%d", rep.SilenceInsertedMS)
	}
}

func TestDamagedVideoDropsUntilVerifiedKeyframe(t *testing.T) {
	video := []sync.Frame{
		{Segment: 0, Present: true, State: model.StateDropped},
		{Segment: 1, Present: true, State: model.StateOriginal},
		{Segment: 2, Present: true, Keyframe: true, Verified: true, State: model.StateOriginal},
		{Segment: 3, Present: true, State: model.StateOriginal},
	}
	rep := sync.Align(video, nil)
	if rep.VideoDropped != 2 {
		t.Fatalf("video dropped=%d, want 2", rep.VideoDropped)
	}
	if got := rep.Decisions[2].VideoAction; got != "resume_verified_keyframe" {
		t.Fatalf("segment 2 action=%q", got)
	}
	if got := rep.Decisions[3].VideoAction; got != "render" {
		t.Fatalf("segment 3 action=%q, want render", got)
	}
}

func TestSkewCorrectionIsBoundedOnRationalTimeline(t *testing.T) {
	video := []sync.Frame{{Segment: 0, Present: true, PTS: clock.FromMilliseconds(100)}}
	audio := []sync.Frame{{Segment: 0, Present: true, PTS: clock.FromMilliseconds(0)}}
	rep := sync.Align(video, audio)
	d := rep.Decisions[0]
	if d.RawSkewMS != 100 {
		t.Fatalf("raw skew=%d, want 100", d.RawSkewMS)
	}
	if d.CorrectionMS != sync.MaxCorrectionMS {
		t.Fatalf("correction=%d, want bound %d", d.CorrectionMS, sync.MaxCorrectionMS)
	}
	if d.CorrectedSkewMS != 20 || rep.AVSkewMS != 20 {
		t.Fatalf("corrected skew=%d report=%d, want 20", d.CorrectedSkewMS, rep.AVSkewMS)
	}
}
