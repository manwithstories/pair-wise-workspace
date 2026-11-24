package timeline

import (
	"errors"
	"testing"

	"mediarun/internal/clock"
	"mediarun/internal/model"
)

func ev(kind model.Kind, track uint32, start clock.Tick, seq uint32, key bool) model.Event {
	return model.Event{Kind: kind, TrackID: track, Start: start, Duration: 1000, Sequence: seq, Keyframe: key}
}

func TestMergeOrdersDeterministically(t *testing.T) {
	inputs := []Input{
		{
			Track: model.Track{ID: 2, Timescale: 90000},
			Events: []model.Event{
				ev(model.KindFrame, 2, 9000, 2, true),
				ev(model.KindFrame, 2, 0, 1, true),
			},
		},
		{
			Track: model.Track{ID: 1, Timescale: 48000},
			Events: []model.Event{
				ev(model.KindSubtitle, 1, 9000, 0, false),
				ev(model.KindChapter, 1, 0, 0, false),
			},
		},
	}
	m, err := Merge(inputs, 90000)
	if err != nil {
		t.Fatal(err)
	}
	// Same tick orders by kind number: chapter(2) < frame(6);
	// subtitle(1) < frame(6) at 9000.
	want := []model.Kind{model.KindChapter, model.KindFrame, model.KindSubtitle, model.KindFrame}
	for i, k := range want {
		if m.Events[i].Kind != k {
			t.Fatalf("event %d kind %v want %v", i, m.Events[i].Kind, k)
		}
	}
	// Reversing the inputs must not change the output.
	rev, err := Merge([]Input{inputs[1], inputs[0]}, 90000)
	if err != nil {
		t.Fatal(err)
	}
	for i := range m.Events {
		if m.Events[i].Digest() != rev.Events[i].Digest() {
			t.Fatalf("order depends on input order at %d", i)
		}
	}
}

func TestMergeRejectsBadInput(t *testing.T) {
	if _, err := Merge(nil, 0); !errors.Is(err, model.ErrInvalid) {
		t.Fatalf("empty inputs: %v", err)
	}
	dup := []Input{
		{Track: model.Track{ID: 1}},
		{Track: model.Track{ID: 1}},
	}
	if _, err := Merge(dup, 0); !errors.Is(err, model.ErrInvalid) {
		t.Fatalf("duplicate track: %v", err)
	}
	neg := []Input{{Track: model.Track{ID: 1}, Events: []model.Event{{Start: -1}}}}
	if _, err := Merge(neg, 0); !errors.Is(err, model.ErrInvalid) {
		t.Fatalf("negative start: %v", err)
	}
}

func TestSegmentsCutAtKeyframes(t *testing.T) {
	var events []model.Event
	for i := 0; i < 8; i++ {
		events = append(events, ev(model.KindFrame, 1, clock.Tick(i*45000), uint32(i), i%2 == 0))
	}
	m, err := Merge([]Input{{Track: model.Track{ID: 1}, Events: events}}, 90000)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Segments) < 3 {
		t.Fatalf("segments %d", len(m.Segments))
	}
	for i, seg := range m.Segments {
		if seg.Ordinal != i+1 {
			t.Fatalf("ordinal %d", seg.Ordinal)
		}
		if len(seg.Events) == 0 {
			t.Fatalf("empty segment %d", i)
		}
		for _, e := range seg.Events {
			if e.Start < seg.Start || (i < len(m.Segments)-1 && e.Start >= seg.End) {
				t.Fatalf("segment %d boundaries %d..%d exclude %d", i, seg.Start, seg.End, e.Start)
			}
		}
	}
	// Re-merging the same events yields identical segments.
	m2, _ := Merge([]Input{{Track: model.Track{ID: 1}, Events: events}}, 90000)
	if len(m2.Segments) != len(m.Segments) {
		t.Fatal("segment count unstable")
	}
	for i := range m.Segments {
		if len(m.Segments[i].Events) != len(m2.Segments[i].Events) {
			t.Fatalf("segment %d differs", i)
		}
	}
}

func TestSimultaneousEventsStayTogether(t *testing.T) {
	inputs := []Input{
		{Track: model.Track{ID: 1}, Events: []model.Event{
			ev(model.KindSEI, 1, 0, 1, true),
			ev(model.KindSEI, 1, 90000, 2, true),
			ev(model.KindSEI, 1, 180000, 3, true),
		}},
		{Track: model.Track{ID: 2}, Events: []model.Event{
			ev(model.KindID3, 2, 0, 1, false),
			ev(model.KindID3, 2, 90000, 2, false),
		}},
	}
	m, err := Merge(inputs, 90000)
	if err != nil {
		t.Fatal(err)
	}
	index := map[clock.Tick]int{}
	for _, seg := range m.Segments {
		for _, e := range seg.Events {
			if prev, ok := index[e.Start]; ok && prev != seg.Ordinal {
				t.Fatalf("tick %d split across segments", e.Start)
			}
			index[e.Start] = seg.Ordinal
		}
	}
}

func TestShortTimelineStillSplits(t *testing.T) {
	events := []model.Event{
		ev(model.KindSubtitle, 1, 0, 1, false),
		ev(model.KindSubtitle, 1, 10, 2, false),
		ev(model.KindSubtitle, 1, 20, 3, false),
	}
	m, err := Merge([]Input{{Track: model.Track{ID: 1}, Events: events}}, clock.TickRate)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Segments) != 3 {
		t.Fatalf("segments %d", len(m.Segments))
	}
}

func TestGapsAttachedToSegments(t *testing.T) {
	events := []model.Event{
		ev(model.KindFrame, 1, 0, 1, true),
		ev(model.KindFrame, 1, 90000, 2, true),
		ev(model.KindFrame, 1, 180000, 3, true),
	}
	gaps := []model.Gap{{TrackID: 1, From: 5, To: 9, Count: 5, Start: 100000, End: 140000}}
	m, err := Merge([]Input{{Track: model.Track{ID: 1}, Events: events, Gaps: gaps}}, 90000)
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, seg := range m.Segments {
		total += seg.GapCount
	}
	if total != 5 {
		t.Fatalf("gap packets %d", total)
	}
	sum := m.Summarize()
	if sum.GapCount != 5 || sum.GapTicks != 40000 {
		t.Fatalf("summary %+v", sum)
	}
	if sum.MissingTracks != 0 {
		t.Fatalf("missing %d", sum.MissingTracks)
	}
}

func TestMissingTrackReported(t *testing.T) {
	m, err := Merge([]Input{
		{Track: model.Track{ID: 1}, Events: []model.Event{ev(model.KindFrame, 1, 0, 1, true)}},
		{Track: model.Track{ID: 2}, Missing: true, Gaps: []model.Gap{{TrackID: 2, Count: 3}}},
	}, clock.TickRate)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Missing) != 1 || m.Missing[0].ID != 2 {
		t.Fatalf("missing: %+v", m.Missing)
	}
	if m.Summarize().MissingTracks != 1 {
		t.Fatal("summary missing")
	}
}

func TestEventLimit(t *testing.T) {
	events := make([]model.Event, MaxEvents+1)
	for i := range events {
		events[i] = model.Event{Kind: model.KindFrame, TrackID: 1, Start: clock.Tick(i)}
	}
	if _, err := Merge([]Input{{Track: model.Track{ID: 1}, Events: events}}, clock.TickRate); !errors.Is(err, model.ErrLimit) {
		t.Fatalf("limit: %v", err)
	}
}

func TestSegmentFor(t *testing.T) {
	events := []model.Event{
		ev(model.KindFrame, 1, 0, 1, true),
		ev(model.KindFrame, 1, 90000, 2, true),
		ev(model.KindFrame, 1, 180000, 3, true),
	}
	m, err := Merge([]Input{{Track: model.Track{ID: 1}, Events: events}}, 90000)
	if err != nil {
		t.Fatal(err)
	}
	seg, ok := m.SegmentFor(95000)
	if !ok || seg.Ordinal != 2 {
		t.Fatalf("segment for 95000: %+v %v", seg, ok)
	}
	if _, ok := m.SegmentFor(-1); ok {
		t.Fatal("negative tick found a segment")
	}
}
