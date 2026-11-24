package graph

import (
	"errors"
	"strings"
	"testing"

	"mediasim/internal/clock"
)

func base() clock.Timeline {
	return clock.Timeline{
		Tick: clock.MustTimebase(1, 1),
		FPS:  clock.MustRational(30, 1),
	}
}

func TestValidTimelineAccepted(t *testing.T) {
	tl, err := New(base(), []Track{{
		ID: "v", Kind: TrackVideo,
		Segments: []Segment{
			{ID: "v0", Start: 0, Duration: 1000, Source: "cam0"},
			{ID: "v1", Start: 1000, Duration: 500, Source: "cam1", EditPoints: []int64{200, 400}},
		},
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if tl.DurationTicks() != 1500 {
		t.Fatalf("duration=%d", tl.DurationTicks())
	}
	if seg, ok := tl.Segment("v1"); !ok || seg.Track != "v" {
		t.Fatalf("lookup=%+v,%v", seg, ok)
	}
}

func TestNegativeDurationAndZeroDenominator(t *testing.T) {
	_, err := New(base(), []Track{{
		ID: "v", Kind: TrackVideo,
		Segments: []Segment{{ID: "v0", Start: 0, Duration: -5, Source: "cam0"}},
	}}, nil)
	if clock.Classify(err) != clock.CatTimeline || !strings.Contains(err.Error(), "negative duration") {
		t.Fatalf("err=%v", err)
	}

	if _, err := clock.NewRational(1, 0); !errors.Is(err, clock.ErrZeroDenominator) {
		t.Fatal("zero denominator accepted")
	}
	_, err = New(clock.Timeline{Tick: clock.MustTimebase(1, 1), FPS: clock.MustRational(1, 1)},
		[]Track{{ID: "v", Kind: "bogus", Segments: []Segment{{ID: "s", Source: "x"}}}}, nil)
	if clock.Classify(err) != clock.CatArgument {
		t.Fatalf("bad kind err=%v", err)
	}
}

func TestEditPointsMustBeStrictlyInside(t *testing.T) {
	cases := [][]int64{{0}, {-1}, {100}, {50, 50}, {80, 20}}
	for _, eps := range cases {
		_, err := New(base(), []Track{{
			ID: "v", Kind: TrackVideo,
			Segments: []Segment{{ID: "v0", Start: 0, Duration: 100, Source: "s", EditPoints: eps}},
		}}, nil)
		if err == nil {
			t.Fatalf("edit points %v accepted", eps)
		}
	}
}

func TestLimitsEnforced(t *testing.T) {
	segs := make([]Segment, 0, MaxSegments+1)
	for i := 0; i <= MaxSegments; i++ {
		segs = append(segs, Segment{ID: "s" + itoa(i), Source: "s"})
	}
	if _, err := New(base(), []Track{{ID: "v", Kind: TrackVideo, Segments: segs}}, nil); err == nil {
		t.Fatal("segment limit not enforced")
	}
	tracks := make([]Track, 0, MaxTracks+1)
	for i := 0; i <= MaxTracks; i++ {
		tracks = append(tracks, Track{ID: "t" + itoa(i), Kind: TrackVideo})
	}
	if _, err := New(base(), tracks, nil); err == nil {
		t.Fatal("track limit not enforced")
	}
}

func TestFreezeCopies(t *testing.T) {
	eps := []int64{10}
	tracks := []Track{{ID: "v", Kind: TrackVideo, Segments: []Segment{{ID: "s", Duration: 100, Source: "x", EditPoints: eps}}}}
	tl, err := New(base(), tracks, nil)
	if err != nil {
		t.Fatal(err)
	}
	eps[0] = 99
	tracks[0].Segments[0].Source = "mutated"
	got := tl.Tracks[0].Segments[0]
	if got.EditPoints[0] != 10 || got.Source != "x" {
		t.Fatalf("timeline aliased caller memory: %+v", got)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
