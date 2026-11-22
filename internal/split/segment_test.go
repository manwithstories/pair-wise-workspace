package split

import (
	"errors"
	"testing"
	"time"

	"mediacq/internal/clock"
	"mediacq/internal/ingest"
	"mediacq/internal/testdata"
)

func parseFixture(t *testing.T, p testdata.Params) (*ingest.Container, []ingest.Event) {
	t.Helper()
	f, err := testdata.Build(p)
	if err != nil {
		t.Fatal(err)
	}
	c, err := ingest.Parse(f.MP4)
	if err != nil {
		t.Fatal(err)
	}
	evs, err := ingest.ParseEvents(f.MP4)
	if err != nil {
		t.Fatal(err)
	}
	return c, evs
}

func TestCutAlignsToKeyframes(t *testing.T) {
	c, _ := parseFixture(t, testdata.DefaultParams(17))
	video := c.Tracks[0]
	plan, err := Cut(video, nil, Config{TargetDurationNS: int64(500 * time.Millisecond)})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Segments) < 2 {
		t.Fatalf("segments = %d, expect multiple", len(plan.Segments))
	}
	// Every segment must start on a keyframe and ranges must tile the track.
	keySet := map[int]bool{}
	for _, k := range video.KeyframeIndices {
		keySet[k] = true
	}
	for i, seg := range plan.Segments {
		if !keySet[seg.SampleStart] {
			t.Fatalf("segment %d starts on non-keyframe %d", i, seg.SampleStart)
		}
		if seg.Range.End <= seg.Range.Start {
			t.Fatalf("segment %d has non-positive range", i)
		}
		if i > 0 && plan.Segments[i-1].Range.End != seg.Range.Start {
			t.Fatal("segments do not form a contiguous timeline")
		}
	}
	last := plan.Segments[len(plan.Segments)-1]
	if last.SampleEnd != len(video.Samples) {
		t.Fatalf("final segment ends at %d, want %d", last.SampleEnd, len(video.Samples))
	}
}

func TestCutVFRUsesActualDurations(t *testing.T) {
	p := testdata.DefaultParams(17)
	p.VFR = true
	c, _ := parseFixture(t, p)
	video := c.Tracks[0]
	plan, err := Cut(video, nil, Config{TargetDurationNS: int64(time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	// Recompute each range from sample starts and confirm the plan matches.
	for _, seg := range plan.Segments {
		wantStart := video.Samples[seg.SampleStart].Start
		var wantEnd clock.Time
		if seg.SampleEnd >= len(video.Samples) {
			last := video.Samples[len(video.Samples)-1]
			wantEnd, _ = last.Start.Add(time.Duration(last.Duration))
		} else {
			wantEnd = video.Samples[seg.SampleEnd].Start
		}
		if seg.Range.Start != wantStart || seg.Range.End != wantEnd {
			t.Fatalf("VFR range mismatch: %+v", seg.Range)
		}
	}
}

func TestBoundaryEventOwnedOnce(t *testing.T) {
	c, evs := parseFixture(t, testdata.DefaultParams(17))
	video := c.Tracks[0]
	plan, err := Cut(video, evs, Config{TargetDurationNS: int64(500 * time.Millisecond)})
	if err != nil {
		t.Fatal(err)
	}
	// Count ownership of every event across segments; each id appears once.
	seen := map[uint32]int{}
	for _, seg := range plan.Segments {
		for _, id := range seg.EventIDs {
			seen[id]++
		}
	}
	// The exact-500ms boundary event must be in exactly one segment.
	boundary := clock.Time(500 * time.Millisecond)
	for _, e := range evs {
		if e.Time != boundary {
			continue
		}
		if seen[e.ID] != 1 {
			t.Fatalf("boundary event %d owned %d times", e.ID, seen[e.ID])
		}
	}
	// No event is double-counted or lost.
	total := 0
	for _, n := range seen {
		if n != 1 {
			t.Fatalf("event owned %d times", n)
		}
		total += n
	}
	if total != countInRange(evs, plan) {
		t.Fatal("event count mismatch after split")
	}
}

func countInRange(evs []ingest.Event, plan *Plan) int {
	n := 0
	for _, e := range evs {
		for _, seg := range plan.Segments {
			if seg.Range.Contains(e.Time) {
				n++
				break
			}
		}
	}
	return n
}

func TestDriftFlagsSegments(t *testing.T) {
	c, _ := parseFixture(t, testdata.DefaultParams(17))
	video := c.Tracks[0]
	window := clock.DriftWindow{
		Range: clock.Range{Start: clock.Time(400 * time.Millisecond), End: clock.Time(900 * time.Millisecond)},
		PPM:   200,
	}
	plan, err := Cut(video, nil, Config{
		TargetDurationNS: int64(500 * time.Millisecond), DriftWindows: []clock.DriftWindow{window},
	})
	if err != nil {
		t.Fatal(err)
	}
	flagged := 0
	for _, seg := range plan.Segments {
		if seg.DriftAffected {
			flagged++
		}
	}
	if flagged == 0 {
		t.Fatal("no segment flagged as drift-affected")
	}
}

func TestCutRejectsTrackWithoutKeyframe(t *testing.T) {
	c, _ := parseFixture(t, testdata.DefaultParams(17))
	video := c.Tracks[0]
	video.KeyframeIndices = nil
	if _, err := Cut(video, nil, Config{TargetDurationNS: int64(time.Second)}); !errors.Is(err, ErrNoKeyframe) {
		t.Fatalf("want ErrNoKeyframe, got %v", err)
	}
}
