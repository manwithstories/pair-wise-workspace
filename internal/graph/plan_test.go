package graph_test

import (
	"testing"

	"archivearchivist/internal/graph"
	"archivearchivist/internal/model"
)

func rat(n, d int64) model.Rational { return model.NewRat(n, d) }

func baseTimeline() *model.Timeline {
	return &model.Timeline{
		Version: 1,
		Tracks: []model.Track{
			{ID: "V1", Kind: model.KindVideo, Codec: "avc1", Source: "v.mp4", Timescale: 90000},
			{ID: "A1", Kind: model.KindAudio, Codec: "mp4a", Source: "a.mp4", Timescale: 48000, DependsOn: []string{"V1"}},
			{ID: "A2", Kind: model.KindAudio, Codec: "mp4a", Source: "b.mp4", Timescale: 48000},
		},
		Clips: []model.Clip{
			{ID: "C1", Track: "V1", Start: rat(0, 1), Duration: rat(1, 1), SourceIn: rat(0, 1)},
			{ID: "C2", Track: "V1", Start: rat(1, 1), Duration: rat(1, 1), SourceIn: rat(0, 1)},
			{ID: "A1C", Track: "A1", Start: rat(0, 1), Duration: rat(2, 1), SourceIn: rat(0, 1)},
		},
		Transitions: []model.Transition{
			{ID: "X1", Kind: "crossfade", From: "C1", To: "C2", Duration: rat(1, 5), Center: rat(1, 1)},
		},
	}
}

func TestBuildOrdersTrackBeforeClip(t *testing.T) {
	tl := baseTimeline()
	g, faults := graph.Build(tl)
	if len(faults) != 0 {
		t.Fatalf("unexpected faults: %v", faults)
	}
	order, stuck := g.TopoOrder()
	if len(stuck) != 0 {
		t.Fatalf("stuck nodes: %v", stuck)
	}
	pos := map[string]int{}
	for i, id := range order {
		pos[id] = i
	}
	if pos["T:V1"] > pos["T:A1"] {
		t.Fatalf("audio scheduled before its video dependency: %v", order)
	}
	if pos["C:C1"] > pos["X:X1"] {
		t.Fatalf("crossfade scheduled before its source clip: %v", order)
	}
}

func TestCycleDetected(t *testing.T) {
	tl := baseTimeline()
	tl.Tracks[2].DependsOn = []string{"A1"}
	tl.Tracks[1].DependsOn = []string{"A2"}
	g, faults := graph.Build(tl)
	if !faults.Has(model.CodeDependencyCycle) {
		t.Fatalf("cycle not reported: %v", faults)
	}
	cycles := g.Cycles()
	if len(cycles) == 0 {
		t.Fatalf("no cycle path returned")
	}
	_, stuck := g.TopoOrder()
	if len(stuck) == 0 {
		t.Fatalf("cycle nodes not excluded from topo order")
	}
}

func TestSelfCycleDetected(t *testing.T) {
	tl := baseTimeline()
	tl.Tracks[0].DependsOn = []string{"V1"}
	_, faults := graph.Build(tl)
	if !faults.Has(model.CodeDependencyCycle) {
		t.Fatalf("self cycle not reported: %v", faults)
	}
}

func TestMissingParentReported(t *testing.T) {
	tl := baseTimeline()
	tl.Clips[1].Parent = "C-DOES-NOT-EXIST"
	// Validation would reject this; Build must still locate it.
	_, faults := graph.Build(tl)
	missing := faults.Filter(model.CodeMissingParent)
	if len(missing) != 1 {
		t.Fatalf("missing parent faults = %v", faults)
	}
	if missing[0].Clip != "C2" {
		t.Fatalf("fault points at %q", missing[0].Clip)
	}
}

func TestAudioUnderCoverageReported(t *testing.T) {
	tl := baseTimeline()
	// Shrink the audio bed to half a second under a one second video clip.
	tl.Clips[2].Duration = rat(1, 2)
	_, faults := graph.Build(tl)
	if !faults.Has(model.CodeAudioUnderCover) {
		t.Fatalf("under-coverage not reported: %v", faults)
	}
}
