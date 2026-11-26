package timeline

import (
	"testing"

	"audiorestore/internal/model"
)

func track(id string, deps ...string) *Track {
	return &Track{ID: id, Rate: 48000, Channels: 2, BitDepth: 16,
		Layout: "stereo_lr", Color: "bt709", DependsOn: deps}
}

func TestTopologicalOrderDeterministic(t *testing.T) {
	tracks := []*Track{
		track("d", "b", "c"),
		track("a"),
		track("c", "a"),
		track("b", "a"),
	}
	g, err := NewGraph(tracks, nil, 48000)
	if err != nil {
		t.Fatalf("NewGraph: %v", err)
	}
	order := g.Order()
	pos := map[string]int{}
	for i, id := range order {
		pos[id] = i
	}
	if !(pos["a"] < pos["b"] && pos["a"] < pos["c"] &&
		pos["b"] < pos["d"] && pos["c"] < pos["d"]) {
		t.Fatalf("dependency order violated: %v", order)
	}
	// Roots with equal rank break ties lexicographically.
	if order[0] != "a" {
		t.Fatalf("expected first track a, got %v", order)
	}
}

func TestUnknownDependencyRejected(t *testing.T) {
	if _, err := NewGraph([]*Track{track("x", "ghost")}, nil, 48000); err == nil {
		t.Fatal("expected error for unknown dependency")
	}
}

func TestCycleRejected(t *testing.T) {
	tracks := []*Track{track("a", "b"), track("b", "a")}
	if _, err := NewGraph(tracks, nil, 48000); err == nil {
		t.Fatal("expected error for dependency cycle")
	}
}

func TestCrossRateConversionIsIntegerAndExact(t *testing.T) {
	// 48 kHz -> 44.1 kHz: 48000 frames map to 44100 exactly.
	got, err := MapFrame(48000, 48000, 44100)
	if err != nil {
		t.Fatal(err)
	}
	if got != 44100 {
		t.Fatalf("48k->44.1k mapping = %d, want 44100", got)
	}
	// Round trip never drifts beyond half a target sample.
	back, err := MapFrame(got, 44100, 48000)
	if err != nil {
		t.Fatal(err)
	}
	if diff := back - 48000; diff < -1 || diff > 1 {
		t.Fatalf("round trip drifted by %d frames", diff)
	}
}

func TestPTSRationalNoFloatDrift(t *testing.T) {
	pts := PTSAt(&Track{ID: "x", Rate: 44100}, 44100)
	if pts.PTS.Num() != 1 || pts.PTS.Den() != 1 {
		t.Fatalf("44100 frames at 44.1 kHz = %s, want 1", pts.PTS)
	}
}

func TestMonotonicCheck(t *testing.T) {
	ok := []TrackPTS{
		{Track: "a", Frame: 0, PTS: model.MustRational(0, 1)},
		{Track: "a", Frame: 1, PTS: model.MustRational(1, 48000)},
		{Track: "a", Frame: 2, PTS: model.MustRational(2, 48000)},
	}
	if err := CheckMonotonic(ok); err != nil {
		t.Fatalf("monotonic sequence flagged: %v", err)
	}
	bad := append(ok, TrackPTS{Track: "a", Frame: 3, PTS: model.MustRational(1, 96000)})
	if err := CheckMonotonic(bad); err == nil {
		t.Fatal("expected non-monotonic error")
	}
}

func TestMappingResidualBoundedByHalfSample(t *testing.T) {
	tr := &Track{ID: "x", Rate: 44100}
	a := Anchor{ID: "v", RefRate: 48000, RefSample: 12345}
	us, err := MappingResidualUS(tr, a)
	if err != nil {
		t.Fatal(err)
	}
	// Half a sample at 44.1 kHz is ~11.34 us.
	if us < 0 || us > 12 {
		t.Fatalf("residual %d us exceeds half a target sample", us)
	}
}

func TestLayoutAndColorConflictFlagged(t *testing.T) {
	tracks := []*Track{
		{ID: "a", Rate: 48000, Layout: "stereo_lr", Color: "bt709"},
		{ID: "b", Rate: 48000, Layout: "mono", Color: "bt2020", DependsOn: []string{"a"}},
		{ID: "c", Rate: 48000, DependsOn: []string{"a"}}, // missing layout
	}
	g, err := NewGraph(tracks, nil, 48000)
	if err != nil {
		t.Fatal(err)
	}
	log := model.NewErrorLog()
	CheckLayout(g, log)
	if !log.Has(model.ErrLayoutConflict) || !log.Has(model.ErrLayoutMissing) {
		t.Fatalf("expected layout conflict and missing entries, got %v", log.All())
	}
	if n := CheckColor(g, log); n != 1 {
		t.Fatalf("expected 1 color conflict, got %d", n)
	}
	// Source mapping must be preserved: layout tags are untouched.
	if g.ByID("b").Layout != "mono" {
		t.Fatal("layout was rewritten instead of merely flagged")
	}
}
