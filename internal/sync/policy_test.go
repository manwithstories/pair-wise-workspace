package sync

import (
	"testing"

	"mediakernel/internal/clock"
)

func TestAlignWithinTolerance(t *testing.T) {
	p := Policy{Tolerance: 20 * clock.Millisecond}
	al, err := p.Align([]TrackTime{
		{Track: 1, Type: 1, Base: 10 * clock.Millisecond, End: 2000 * clock.Millisecond, Keys: []clock.Nanos{0, 500 * clock.Millisecond}},
		{Track: 2, Type: 2, Base: 0, End: 2000 * clock.Millisecond},
	})
	if err != nil {
		t.Fatal(err)
	}
	if al.Drift != 10*clock.Millisecond {
		t.Fatalf("drift=%d", al.Drift)
	}
	if len(al.Corrections) != 0 {
		t.Fatalf("unexpected corrections: %v", al.Corrections)
	}
	if al.Start != 0 || al.End != 2000*clock.Millisecond {
		t.Fatalf("start=%d end=%d", al.Start, al.End)
	}
}

func TestAlignSnapsToKeyframe(t *testing.T) {
	p := Policy{Tolerance: 20 * clock.Millisecond}
	al, err := p.Align([]TrackTime{
		{Track: 1, Type: 1, Base: 495 * clock.Millisecond, End: 2000 * clock.Millisecond,
			Keys: []clock.Nanos{0, 450 * clock.Millisecond, 900 * clock.Millisecond}},
		{Track: 2, Type: 2, Base: 460 * clock.Millisecond, End: 2000 * clock.Millisecond},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(al.Corrections) != 1 {
		t.Fatalf("corrections=%v", al.Corrections)
	}
	c := al.Corrections[0]
	if c.To != 450*clock.Millisecond || c.Track != 1 {
		t.Fatalf("correction: %+v", c)
	}
	if al.Bases[1] != 450*clock.Millisecond {
		t.Fatalf("video base=%d", al.Bases[1])
	}
	if al.Start != 450*clock.Millisecond {
		t.Fatalf("start=%d", al.Start)
	}
}

func TestAlignAudioOnly(t *testing.T) {
	p := Policy{Tolerance: 20 * clock.Millisecond}
	al, err := p.Align([]TrackTime{
		{Track: 2, Type: 2, Base: 5 * clock.Millisecond, End: 1000 * clock.Millisecond},
	})
	if err != nil {
		t.Fatal(err)
	}
	if al.Start != 5*clock.Millisecond || al.Drift != 0 {
		t.Fatalf("align: %+v", al)
	}
}

func TestAlignEmpty(t *testing.T) {
	p := Policy{Tolerance: 20 * clock.Millisecond}
	if _, err := p.Align(nil); err == nil {
		t.Fatal("want error for no tracks")
	}
}
