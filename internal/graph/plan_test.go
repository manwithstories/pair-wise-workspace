package graph

import (
	"errors"
	"strings"
	"testing"

	"mediasim/internal/clock"
)

func multiTrack(t *testing.T) *Timeline {
	t.Helper()
	tl, err := New(base(), []Track{
		{ID: "v", Kind: TrackVideo, Segments: []Segment{
			{ID: "v0", Start: 0, Duration: 1000, Source: "cam0", EditPoints: []int64{400}},
			{ID: "v1", Start: 1000, Duration: 500, Source: "cam1"},
		}},
		{ID: "a", Kind: TrackAudio, Segments: []Segment{
			{ID: "a0", Start: 0, Duration: 1500, Source: "voice"},
		}},
	}, []Transition{{ID: "xf", From: "v0", To: "v1", Kind: TransCut, Duration: 0}})
	if err != nil {
		t.Fatal(err)
	}
	return tl
}

func assertDeps(t *testing.T, g *Graph, node NodeID, want ...NodeID) {
	t.Helper()
	got := g.Deps(node)
	if len(got) != len(want) {
		t.Fatalf("%s deps=%v want %v", node, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s deps=%v want %v", node, got, want)
		}
	}
}

func TestBuildDerivesEdges(t *testing.T) {
	tl := multiTrack(t)
	g, err := Build(tl)
	if err != nil {
		t.Fatal(err)
	}
	// v1 depends on v0 (same track order).
	assertDeps(t, g, "seg:v1", "seg:v0")
	// The edit piece of v0 depends on the whole segment.
	assertDeps(t, g, "piece:v0:0", "seg:v0")
	assertDeps(t, g, "piece:v0:1", "seg:v0")
	// A transition depends on both endpoints.
	assertDeps(t, g, "tr:xf", "seg:v0", "seg:v1")
	// The audio segment has no predecessor.
	assertDeps(t, g, "seg:a0")
	if n, ok := g.Node("tr:xf"); !ok || n.Start != 1000 || n.Duration != 0 {
		t.Fatalf("transition node=%+v ok=%v", n, ok)
	}
}

func TestMissingDependencyReported(t *testing.T) {
	tl, err := New(base(), []Track{{
		ID: "v", Kind: TrackVideo,
		Segments: []Segment{{ID: "v0", Start: 0, Duration: 100, Source: "s", Requires: []string{"nope"}}},
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Build(tl)
	if clock.Classify(err) != clock.CatGraph || !strings.Contains(err.Error(), "requires missing segment") {
		t.Fatalf("err=%v", err)
	}
	if sp := spanOf(t, err); sp.Track != "v" || sp.Segment != "v0" {
		t.Fatalf("span=%+v", sp)
	}
}

func spanOf(t *testing.T, err error) clock.Span {
	t.Helper()
	var ce *clock.Error
	if !errors.As(err, &ce) {
		t.Fatalf("not classified: %v", err)
	}
	return ce.Span
}
