package timeline

import (
	"testing"

	"mediasync/internal/clock"
	"mediasync/internal/subtitle"
)

func TestRateConflictRuling(t *testing.T) {
	m := NewMerger()
	m.AddChunk(nil, []Marker{
		{Rate: clock.FPS24, Nanos: 0, Label: "a"},
		{Rate: clock.FPS2997, Nanos: 1_001_000_000, Label: "b"},
	}, 1<<62)
	if len(m.Decisions) != 1 || m.Decisions[0].Kind != "rate_conflict" {
		t.Fatalf("decisions = %+v", m.Decisions)
	}
	if len(m.Defects) != 1 {
		t.Fatalf("defects = %+v", m.Defects)
	}
}

func TestSpanChunkRuling(t *testing.T) {
	m := NewMerger()
	cues := []subtitle.Cue{
		{Index: 1, Start: 0, End: 2_500_000_000, Text: "x"},
		{Index: 2, Start: 2_000_000_000, End: 4_500_000_000, Text: "y"},
	}
	evs := m.AddChunk(cues, nil, 2_000_000_001)
	if len(evs) != 2 {
		t.Fatalf("events = %d", len(evs))
	}
	// Both cues stay active past the horizon: two rulings.
	if len(m.Decisions) != 2 || m.Decisions[0].Kind != "span_chunk" {
		t.Fatalf("decisions = %+v", m.Decisions)
	}
}

func TestSummaryDeterministic(t *testing.T) {
	build := func() string {
		m := NewMerger()
		m.AddChunk([]subtitle.Cue{{Index: 1, Start: 0, End: 1, Text: "a"}}, []Marker{
			{Rate: clock.FPS25, Nanos: 5, Label: "m"},
		}, 1<<62)
		return m.Summary()
	}
	if build() != build() {
		t.Fatal("summary not deterministic")
	}
}
