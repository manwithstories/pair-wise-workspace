package model

import (
	"testing"

	"medialine/internal/fault"
)

func mkTrack(id int, starts ...int64) *Track {
	tr := &Track{ID: id, Kind: KindVideo, Timescale: 90000}
	for i, s := range starts {
		tr.Samples = append(tr.Samples, Sample{Number: i + 1, DTSUS: s, PTSUS: s, DurationUS: 1000, Keyframe: i == 0})
	}
	return tr
}

func TestTrackDurationAndKeyframes(t *testing.T) {
	tr := mkTrack(1, 0, 1000, 2000)
	if got := tr.DurationUS(); got != 3000 {
		t.Fatalf("duration=%d", got)
	}
	kf := tr.KeyframeIndex()
	if len(kf) != 1 || kf[0] != 1 {
		t.Fatalf("keyframes=%v", kf)
	}
}

func TestTrackValidate(t *testing.T) {
	ok := mkTrack(1, 0, 1000, 2000)
	if err := ok.Validate(); err != nil {
		t.Fatalf("unexpected: %v", err)
	}

	back := mkTrack(2, 0, 2000, 1000)
	fe, isFault := fault.As(back.Validate())
	if !isFault || fe.Kind != fault.KindBackwardsTime || fe.TrackID != 2 || fe.Offset != 2 {
		t.Fatalf("wrong fault: %v", back.Validate())
	}

	dup := &Track{ID: 3, Samples: []Sample{{Number: 1}, {Number: 2}}}
	fe, _ = fault.As(dup.Validate())
	if fe.Kind != fault.KindDuplicateTime || fe.Offset != 1 {
		t.Fatalf("wrong dup fault: %v", fe)
	}

	neg := &Track{ID: 4, Samples: []Sample{{Number: 1, DurationUS: -5}}}
	fe, _ = fault.As(neg.Validate())
	if fe.Kind != fault.KindZeroDuration {
		t.Fatalf("wrong neg fault: %v", fe)
	}
}

func TestTimelineValidate(t *testing.T) {
	base := func() *Timeline {
		return &Timeline{Tracks: []*Track{mkTrack(1, 0, 1000), mkTrack(2, 0, 1000)}}
	}
	if err := base().Validate(); err != nil {
		t.Fatalf("unexpected: %v", err)
	}

	tl := base()
	tl.Tracks = append(tl.Tracks, mkTrack(1, 0))
	if err := tl.Validate(); err == nil {
		t.Fatal("expected duplicate track error")
	}

	tl = base()
	tl.Tracks[0].ID = 0
	if err := tl.Validate(); err == nil {
		t.Fatal("expected non-positive id error")
	}

	tl = base()
	tl.Cuts = []Cut{{AtUS: -1}}
	if err := tl.Validate(); err == nil {
		t.Fatal("expected negative cut error")
	}

	tl = base()
	tl.Cuts = []Cut{{AtUS: 10}, {AtUS: 10}}
	if err := tl.Validate(); err == nil {
		t.Fatal("expected duplicate cut error")
	}

	tl = base()
	tl.Cuts = []Cut{{AtUS: 10, Transition: &Transition{StartUS: 10, EndUS: 5, Sources: []int{1, 2}}}}
	if err := tl.Validate(); err == nil {
		t.Fatal("expected bad transition error")
	}

	tl = base()
	tl.Cuts = []Cut{{AtUS: 10, Transition: &Transition{StartUS: 10, EndUS: 20, Sources: []int{9}}}}
	if err := tl.Validate(); err == nil {
		t.Fatal("expected unknown track in transition")
	}
}

func TestSpan(t *testing.T) {
	tl := &Timeline{Tracks: []*Track{mkTrack(1, 0, 5000), mkTrack(2, 0, 9000)}}
	if got := tl.Span(); got != 10000 {
		t.Fatalf("span=%d", got)
	}
	tl.DurationUS = 42
	if got := tl.Span(); got != 42 {
		t.Fatalf("span=%d", got)
	}
}

func TestSegmentIDStable(t *testing.T) {
	a := SegmentID("v720", 2, 0, 2_000_000)
	b := SegmentID("v720", 2, 0, 2_000_000)
	if a != b {
		t.Fatalf("ids differ: %s vs %s", a, b)
	}
	if a == SegmentID("v1080", 2, 0, 2_000_000) {
		t.Fatal("rep should affect id")
	}
}

func TestTransitionValidate(t *testing.T) {
	known := map[int]bool{1: true, 2: true}
	tr := &Transition{StartUS: 0, EndUS: 10, Sources: []int{1, 2}}
	if err := tr.Validate(known); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	empty := &Transition{StartUS: 0, EndUS: 10}
	if err := empty.Validate(known); err == nil {
		t.Fatal("expected empty sources error")
	}
}
