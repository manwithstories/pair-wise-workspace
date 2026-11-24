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

func TestCycleDetectedWithPath(t *testing.T) {
	tl, err := New(base(), []Track{{
		ID: "v", Kind: TrackVideo,
		Segments: []Segment{
			{ID: "a", Start: 0, Duration: 100, Source: "s", Requires: []string{"c"}},
			{ID: "b", Start: 100, Duration: 100, Source: "s", Requires: []string{"a"}},
			{ID: "c", Start: 200, Duration: 100, Source: "s", Requires: []string{"b"}},
		},
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Build(tl)
	if clock.Classify(err) != clock.CatGraph || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("err=%v", err)
	}
	if !strings.Contains(err.Error(), "->") {
		t.Fatalf("cycle path missing: %v", err)
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

func TestPlanIsDeterministicAndRespectsDeps(t *testing.T) {
	tl := multiTrack(t)
	p1, err := BuildPlan(tl)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := BuildPlan(tl)
	if err != nil {
		t.Fatal(err)
	}
	if len(p1.Steps) != len(p2.Steps) {
		t.Fatal("plan length changed between runs")
	}
	for i := range p1.Steps {
		if p1.Steps[i] != p2.Steps[i] {
			t.Fatalf("step %d differs:\n%+v\n%+v", i, p1.Steps[i], p2.Steps[i])
		}
	}
	pos := make(map[NodeID]int)
	for i, s := range p1.Steps {
		pos[s.Node] = i
	}
	for _, s := range p1.Steps {
		for _, d := range g(t, tl).Deps(s.Node) {
			if pos[d] >= pos[s.Node] {
				t.Fatalf("%s scheduled before its dependency %s", s.Node, d)
			}
		}
	}
}

func g(t *testing.T, tl *Timeline) *Graph {
	t.Helper()
	gr, err := Build(tl)
	if err != nil {
		t.Fatal(err)
	}
	return gr
}

func TestSameInstantOrderingIsStable(t *testing.T) {
	// Two tracks with segments starting at exactly the same tick.
	tl, err := New(base(), []Track{
		{ID: "a", Kind: TrackAudio, Segments: []Segment{{ID: "a0", Start: 0, Duration: 100, Source: "s"}}},
		{ID: "b", Kind: TrackMarker, Segments: []Segment{{ID: "b0", Start: 0, Duration: 100, Source: "s"}}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	p, err := BuildPlan(tl)
	if err != nil {
		t.Fatal(err)
	}
	// Same instant resolves by track id: a's segment and piece before b's.
	var tracks []string
	for _, s := range p.Steps {
		if s.Kind == NodeSegment {
			tracks = append(tracks, s.Track)
		}
	}
	if len(tracks) != 2 || tracks[0] != "a" || tracks[1] != "b" {
		t.Fatalf("segment order=%v from %v", tracks, p.Steps)
	}
}
