package cut

import "testing"

func TestPlanAlignsFrameAndSample(t *testing.T) {
	// 30 fps => 3000 ticks/frame; 48 kHz audio.
	p := NewPlanner(TrackSpec{FrameRateNum: 30, FrameRateDen: 1, SampleRate: 48000})
	bounds, err := p.Plan([]Request{
		{Label: "a", At90k: 7500},
		{Label: "b", At90k: 90000},
	})
	if err != nil {
		t.Fatal(err)
	}
	if bounds[0].Video90k != 6000 {
		t.Fatalf("video align = %d, want 6000", bounds[0].Video90k)
	}
	if bounds[0].Audio90k > 7500 {
		t.Fatalf("audio aligned up: %d", bounds[0].Audio90k)
	}
	if bounds[0].Cut90k != min64(bounds[0].Video90k, bounds[0].Audio90k) {
		t.Fatal("cut must be the earlier aligned edge")
	}
	// 1s is an exact frame boundary.
	if bounds[1].Video90k != 90000 {
		t.Fatalf("1s video = %d, want 90000", bounds[1].Video90k)
	}
}

func TestPlanNeverMovesBackwardsOnReset(t *testing.T) {
	p := NewPlanner(TrackSpec{FrameRateNum: 25, FrameRateDen: 1, SampleRate: 48000})
	// Stream-arrival order includes a backward jump simulating a PTS reset.
	stream := []Request{
		{Label: "r0", At90k: 180000},
		{Label: "r1", At90k: 360000},
		{Label: "reset", At90k: 5000},
		{Label: "r2", At90k: 540000},
	}
	var bounds []Boundary
	prev := int64(-1)
	for _, req := range stream {
		b := p.Advance(req)
		if b.Cut90k < prev {
			t.Fatalf("cut moved backwards: %d < %d", b.Cut90k, prev)
		}
		prev = b.Cut90k
		bounds = append(bounds, b)
	}
	var reset *Boundary
	for i := range bounds {
		if bounds[i].Label == "reset" {
			reset = &bounds[i]
		}
	}
	if reset == nil {
		t.Fatal("reset boundary missing")
	}
	if reset.Cut90k < 360000 {
		t.Fatalf("reset cut %d regressed below 360000", reset.Cut90k)
	}
}

func TestPlanCollapsesDuplicates(t *testing.T) {
	p := NewPlanner(TrackSpec{FrameRateNum: 30, FrameRateDen: 1, SampleRate: 48000})
	bounds, err := p.Plan([]Request{
		{Label: "x", At90k: 90000},
		{Label: "y", At90k: 90000},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(bounds) != 1 {
		t.Fatalf("duplicates not collapsed: %d boundaries", len(bounds))
	}
}

func TestPlanSortsOutOfOrderRequests(t *testing.T) {
	p := NewPlanner(TrackSpec{FrameRateNum: 30, FrameRateDen: 1, SampleRate: 48000})
	bounds, err := p.Plan([]Request{
		{Label: "late", At90k: 180000},
		{Label: "early", At90k: 90000},
	})
	if err != nil {
		t.Fatal(err)
	}
	if bounds[0].Label != "early" || bounds[1].Label != "late" {
		t.Fatalf("not sorted: %s then %s", bounds[0].Label, bounds[1].Label)
	}
}
