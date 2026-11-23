package track

import (
	"testing"

	"mediad/diag"
)

func TestTopoOrderRespectsDeps(t *testing.T) {
	g, err := NewGraph([]Track{
		{ID: "video-a", Kind: KindRTP, Deps: []string{"tc"}},
		{ID: "tc", Kind: KindTimecode},
		{ID: "sub", Kind: KindSRT, Deps: []string{"tc"}},
		{ID: "video-b", Kind: KindRTP, Deps: []string{"video-a", "sub"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	order, cyc, err := g.Topo()
	if err != nil || cyc != nil {
		t.Fatalf("unexpected cycle/error: %v %v", cyc, err)
	}
	pos := map[string]int{}
	for i, id := range order {
		pos[id] = i
	}
	for _, e := range g.Edges() {
		if pos[e.From] >= pos[e.To] {
			t.Fatalf("edge %s -> %s violated in order %v", e.From, e.To, order)
		}
	}
	// Deterministic tie-break: tc first, then alphabetical sub < video-a.
	want := []string{"tc", "sub", "video-a", "video-b"}
	if len(order) != len(want) {
		t.Fatalf("order = %v", order)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}

func TestCycleLocatedWithEdge(t *testing.T) {
	g, err := NewGraph([]Track{
		{ID: "a", Kind: KindRTP, Deps: []string{"c"}},
		{ID: "b", Kind: KindRTP, Deps: []string{"a"}},
		{ID: "c", Kind: KindRTP, Deps: []string{"b"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, cyc, err := g.Topo()
	if diag.CodeOf(err) != diag.EGraphCycle {
		t.Fatalf("want E_GRAPH_CYCLE, got %v", err)
	}
	if cyc == nil || cyc.ClosingEdge.From == "" {
		t.Fatalf("cycle detail missing: %+v", cyc)
	}
	// The cycle must contain all three vertices and close on a real edge.
	seen := map[string]bool{}
	for _, id := range cyc.Path {
		seen[id] = true
	}
	if !seen["a"] || !seen["b"] || !seen["c"] {
		t.Fatalf("cycle path %v does not cover all vertices", cyc.Path)
	}
	found := false
	for _, e := range g.Edges() {
		if e == cyc.ClosingEdge {
			found = true
		}
	}
	if !found {
		t.Fatalf("closing edge %+v is not a real graph edge", cyc.ClosingEdge)
	}
	if cyc.Path[0] != cyc.Path[len(cyc.Path)-1] {
		t.Fatalf("cycle path must repeat first vertex at end: %v", cyc.Path)
	}
}

func TestSelfLoop(t *testing.T) {
	g, err := NewGraph([]Track{{ID: "x", Kind: KindRTP, Deps: []string{"x"}}})
	if err != nil {
		t.Fatal(err)
	}
	_, cyc, err := g.Topo()
	if diag.CodeOf(err) != diag.EGraphCycle {
		t.Fatalf("self loop must be a cycle, got %v", err)
	}
	if cyc.ClosingEdge != (Edge{From: "x", To: "x"}) {
		t.Fatalf("self-loop edge = %+v", cyc.ClosingEdge)
	}
}

func TestMissingAndDuplicateRejected(t *testing.T) {
	if _, err := NewGraph([]Track{
		{ID: "a", Kind: KindRTP, Deps: []string{"ghost"}},
	}); diag.CodeOf(err) != diag.EGraphMissing {
		t.Fatalf("missing dep must be E_GRAPH_MISSING, got %v", err)
	}
	if _, err := NewGraph([]Track{
		{ID: "a", Kind: KindRTP},
		{ID: "a", Kind: KindSRT},
	}); diag.CodeOf(err) != diag.EParam {
		t.Fatalf("duplicate must be E_PARAM, got %v", err)
	}
	if _, err := NewGraph([]Track{{ID: "", Kind: KindRTP}}); diag.CodeOf(err) != diag.EParam {
		t.Fatalf("empty ID must be E_PARAM, got %v", err)
	}
}

func TestEmptyGraphTopo(t *testing.T) {
	g, err := NewGraph(nil)
	if err != nil {
		t.Fatal(err)
	}
	order, cyc, err := g.Topo()
	if err != nil || cyc != nil || len(order) != 0 {
		t.Fatalf("empty graph: order=%v cyc=%v err=%v", order, cyc, err)
	}
}
