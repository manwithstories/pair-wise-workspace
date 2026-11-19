package graph_test

import (
	"testing"

	"archivearchivist/internal/graph"
)

func TestDependentClosureIsolatesBranch(t *testing.T) {
	tl := baseTimeline()
	g, _ := graph.Build(tl)
	closure := g.DependentClosure("T:V1")
	set := map[string]bool{}
	for _, id := range closure {
		set[id] = true
	}
	if !set["T:A1"] {
		t.Fatalf("audio not in video closure: %v", closure)
	}
	if set["T:A2"] {
		t.Fatalf("independent audio wrongly in video closure: %v", closure)
	}
}

func TestNoCycleOnLinearGraph(t *testing.T) {
	tl := baseTimeline()
	g, _ := graph.Build(tl)
	if got := g.Cycles(); len(got) != 0 {
		t.Fatalf("false cycle: %v", got)
	}
}
