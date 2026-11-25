package clock

import (
	"testing"

	"mediaaudit/internal/model"
)

func TestFitClockDriftPPM(t *testing.T) {
	// Nominal RTP 90kHz tick = 1e9/90000 ns. Device runs 180ppm fast, so each
	// tick occupies nominal*(1-180e-6) ns.
	nominal := 1e9 / 90000.0
	slope := nominal * (1 - 180e-6)
	var anchors []Anchor
	for i := int64(0); i < 100; i++ {
		tick := i * 900 // 10ms of ticks
		anchors = append(anchors, Anchor{
			DeviceTicks: tick,
			WallNanos:   int64(slope*float64(tick) + 0.5),
		})
	}
	fit, err := FitClock(anchors, nominal)
	if err != nil {
		t.Fatal(err)
	}
	diff := fit.PPM - 180
	if diff < -1 || diff > 1 {
		t.Fatalf("PPM=%.3f want ~180", fit.PPM)
	}
}

func TestSingleAnchorOffsetOnly(t *testing.T) {
	nominal := 1e9 / 48000.0
	fit, err := FitClock([]Anchor{{DeviceTicks: 48000, WallNanos: 5_000_000_000}}, nominal)
	if err != nil {
		t.Fatal(err)
	}
	if !fit.DriftOnly {
		t.Fatal("single anchor must be offset-only")
	}
	// tick 0 wall = 5e9 - 1s.
	if got := fit.ToWall(0); got != 4_000_000_000 {
		t.Fatalf("wall(0)=%d want 4e9", got)
	}
}

func TestTimelineDeterministicOrder(t *testing.T) {
	tl := &Timeline{}
	mk := func(ns int64, track string, f model.Frame) Event {
		return Event{WallNanos: ns, Track: track, Frame: f}
	}
	tl.Add(mk(100, "b", 5))
	tl.Add(mk(50, "a", 1))
	tl.Add(mk(100, "a", 9))
	tl.Add(mk(100, "a", 3)) // tie ns+track -> order by frame
	ev := tl.Events()
	want := []struct {
		ns    int64
		track string
		frame model.Frame
	}{
		{50, "a", 1}, {100, "a", 3}, {100, "a", 9}, {100, "b", 5},
	}
	if len(ev) != len(want) {
		t.Fatalf("len=%d want %d", len(ev), len(want))
	}
	for i, w := range want {
		if ev[i].WallNanos != w.ns || ev[i].Track != w.track || ev[i].Frame != w.frame {
			t.Fatalf("idx %d = %+v want %+v", i, ev[i], w)
		}
	}
	// Repeated calls must yield identical ordering.
	ev2 := tl.Events()
	for i := range ev {
		if ev[i] != ev2[i] {
			t.Fatal("timeline ordering not deterministic")
		}
	}
}

func TestMapFrameAndAlignRange(t *testing.T) {
	audio := model.Hz(48000)
	video := model.Hz(24)
	// 1 second of audio frames -> 24 video frames.
	if got := MapFrame(48000, audio, video); got != 24 {
		t.Fatalf("MapFrame=%d want 24", got)
	}
	r := model.FrameRange{First: 48000, Last: 48032}
	al := AlignRange(r, audio, video)
	if al.First != 24 || al.Last < 24 {
		t.Fatalf("aligned range wrong: %+v", al)
	}
}

func TestProjectDamage(t *testing.T) {
	audio := model.Hz(48000)
	video := model.Hz(24)
	src := Event{WallNanos: 1_000_000_000, Track: "wav", Frame: 48000, Kind: model.KindCRC, ByteOffset: 100}
	targets := []TrackRate{{Track: "wav", Rate: audio}, {Track: "mp4", Rate: video}}
	dm := ProjectDamage(src, audio, targets)
	if len(dm.Targets) != 2 {
		t.Fatalf("targets=%d want 2", len(dm.Targets))
	}
	if dm.Targets[1].Track != "mp4" || dm.Targets[1].Frame != 24 {
		t.Fatalf("projection wrong: %+v", dm.Targets[1])
	}
}
