package model

import "testing"

func TestClockToNsTable(t *testing.T) {
	cases := []struct {
		ticks int64
		scale uint32
		want  int64
	}{
		{0, 90000, 0},
		{90000, 90000, 1_000_000_000},
		{3000, 90000, 33_333_333},
		{1, 90000, 11111},
		{1024, 48000, 21_333_333},
		{1, 1_000_000_000, 1},
		{25, 25, 1_000_000_000},
		{-90000, 90000, -1_000_000_000},
		{5, 0, 0},
		{1 << 62, 90000, 3_155_760_000_000_000_000},
		{-(1 << 62), 90000, -3_155_760_000_000_000_000},
	}
	for _, c := range cases {
		if got := ClockToNs(c.ticks, c.scale); got != c.want {
			t.Errorf("ClockToNs(%d,%d)=%d want %d", c.ticks, c.scale, got, c.want)
		}
	}
}

func TestNsToClockRoundTrip(t *testing.T) {
	for _, scale := range []uint32{90000, 48000, 25, 1_000_000_000} {
		for _, ns := range []int64{0, 1_000_000_000, 119_960_000_000} {
			ticks := NsToClock(ns, scale)
			if back := ClockToNs(ticks, scale); back != ns {
				t.Errorf("scale %d ns %d -> %d ticks -> %d ns", scale, ns, ticks, back)
			}
		}
	}
	if got := NsToClock(123, 0); got != 0 {
		t.Errorf("zero timescale gave %d", got)
	}
}

func TestValidate(t *testing.T) {
	good := &Timeline{Tracks: map[uint32]*TrackTimeline{
		1: {TrackID: 1, PTS: []int64{0, 10, 20}, Keyframes: []int{0}},
	}}
	if err := good.Validate(); err != nil {
		t.Fatalf("valid timeline rejected: %v", err)
	}
	bad := &Timeline{Tracks: map[uint32]*TrackTimeline{
		1: {TrackID: 1, PTS: []int64{0, 20, 10}},
	}}
	if err := bad.Validate(); err == nil {
		t.Fatal("regressing program time accepted")
	}
	badKF := &Timeline{Tracks: map[uint32]*TrackTimeline{
		1: {TrackID: 1, PTS: []int64{0, 10}, Keyframes: []int{5}},
	}}
	if err := badKF.Validate(); err == nil {
		t.Fatal("out-of-range keyframe accepted")
	}
}

func TestSortedTracksAndMaxPTS(t *testing.T) {
	tl := &Timeline{
		Order: []uint32{3, 1},
		Tracks: map[uint32]*TrackTimeline{
			1: {TrackID: 1, PTS: []int64{0, 5}},
			2: {TrackID: 2, PTS: []int64{0, 99}},
			3: {TrackID: 3, PTS: []int64{0, 7}},
		},
	}
	got := tl.SortedTracks()
	var ids []uint32
	for _, tr := range got {
		ids = append(ids, tr.TrackID)
	}
	want := []uint32{3, 1, 2}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("order=%v want %v", ids, want)
		}
	}
	if tl.MaxPTS() != 99 {
		t.Fatalf("max pts=%d", tl.MaxPTS())
	}
}
