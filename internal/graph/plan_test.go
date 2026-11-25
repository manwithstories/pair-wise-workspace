package graph

import (
	"testing"

	"fieldcut/internal/clock"
)

func TestTopoSortStable(t *testing.T) {
	nodes := map[string]Node{
		"manifest":    {ID: "manifest", DependsOn: []string{"cut", "fingerprint"}},
		"cut":         {ID: "cut", DependsOn: []string{"align", "audio"}},
		"audio":       {ID: "audio", DependsOn: []string{"ingest"}},
		"align":       {ID: "align", DependsOn: []string{"ingest"}},
		"fingerprint": {ID: "fingerprint", DependsOn: []string{"cut"}},
		"ingest":      {ID: "ingest"},
	}
	first, err := TopoSort(nodes)
	if err != nil {
		t.Fatal(err)
	}
	second, err := TopoSort(nodes)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 6 || stringsJoin(first) != stringsJoin(second) {
		t.Fatalf("order unstable: %v vs %v", first, second)
	}
	pos := map[string]int{}
	for i, id := range first {
		pos[id] = i
	}
	for id, n := range nodes {
		for _, dep := range n.DependsOn {
			if pos[dep] >= pos[id] {
				t.Fatalf("dependency %s must precede %s; order=%v", dep, id, first)
			}
		}
	}
	// Lexicographic ready-node tie break makes align precede audio.
	if pos["align"] >= pos["audio"] {
		t.Fatalf("expected stable tie break, order=%v", first)
	}
}

func stringsJoin(xs []string) string {
	out := ""
	for _, x := range xs {
		out += x + "|"
	}
	return out
}

func TestTopoSortCycleFails(t *testing.T) {
	_, err := NewPlan(
		Node{ID: "a", DependsOn: []string{"b"}},
		Node{ID: "b", DependsOn: []string{"c"}},
		Node{ID: "c", DependsOn: []string{"a"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := NewPlan(
		Node{ID: "a", DependsOn: []string{"b"}},
		Node{ID: "b", DependsOn: []string{"c"}},
		Node{ID: "c", DependsOn: []string{"a"}},
	)
	_, err = p.TopoSort()
	if err == nil {
		t.Fatal("expected cycle error")
	}
	ce, ok := clock.AsError(err)
	if !ok || ce.Code != clock.CodeCycle {
		t.Fatalf("expected structured cycle, got %v", err)
	}
}

func TestColorConflictPolicy(t *testing.T) {
	descs := []ColorDescription{
		{TrackID: 1, Primaries: "bt709", Transfer: "bt709", Matrix: "bt709", HasMetadata: true},
		{TrackID: 2, Primaries: "bt2020", Transfer: "smpte2084", Matrix: "bt2020nc", HasMetadata: true},
	}
	if _, err := ResolveColor(descs, ColorPolicy{Mode: "strict"}); err == nil {
		t.Fatal("strict policy must reject conflict")
	} else if ce, ok := clock.AsError(err); !ok || ce.Code != clock.CodeColorConflict {
		t.Fatalf("expected color_conflict, got %v", err)
	}
	decision, err := ResolveColor(descs, ColorPolicy{Mode: "prefer_track", PreferredTrackID: 2})
	if err != nil {
		t.Fatal(err)
	}
	if decision.Selected.TrackID != 2 || len(decision.Conflicts) != 1 || decision.Reason == "" {
		t.Fatalf("bad decision: %#v", decision)
	}
}
