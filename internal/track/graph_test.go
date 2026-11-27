package track

import (
	"errors"
	"strings"
	"testing"

	"fastpost/internal/model"
)

func build(t *testing.T, specs [][2]string) *Graph {
	t.Helper()
	g := NewGraph()
	known := map[string]bool{}
	add := func(id string) {
		if known[id] {
			return
		}
		if err := g.Add(Node{ID: id, Kind: KindSource}); err != nil {
			t.Fatalf("add %s: %v", id, err)
		}
		known[id] = true
	}
	for _, e := range specs {
		add(e[0])
		add(e[1])
	}
	for _, e := range specs {
		n := g.nodes[e[1]]
		n.Deps = append(n.Deps, e[0])
		g.edges[e[0]] = append(g.edges[e[0]], e[1])
	}
	return g
}

func TestTopoStableOrder(t *testing.T) {
	g := NewGraph()
	for _, id := range []string{"a", "b", "c", "d"} {
		if err := g.Add(Node{ID: id, Kind: KindSource}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := g.Topo()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a", "b", "c", "d"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("topo = %v, want %v", got, want)
	}
}

func TestTopoDiamond(t *testing.T) {
	// s -> a -> m, s -> b -> m, then m -> p
	g := build(t, [][2]string{
		{"s", "a"}, {"s", "b"}, {"a", "m"}, {"b", "m"}, {"m", "p"},
	})
	got, err := g.Topo()
	if err != nil {
		t.Fatal(err)
	}
	pos := map[string]int{}
	for i, id := range got {
		pos[id] = i
	}
	for _, e := range [][2]string{
		{"s", "a"}, {"s", "b"}, {"a", "m"}, {"b", "m"}, {"m", "p"},
	} {
		if pos[e[0]] >= pos[e[1]] {
			t.Fatalf("edge %s->%s violated in %v", e[0], e[1], got)
		}
	}
	if got[0] != "s" || got[len(got)-1] != "p" {
		t.Fatalf("unexpected diamond order %v", got)
	}
}

func TestCycleDetected(t *testing.T) {
	// Build a cycle by hand since Add rejects unknown deps.
	g := NewGraph()
	ids := []string{"x", "y", "z"}
	for _, id := range ids {
		if err := g.Add(Node{ID: id, Kind: KindSource}); err != nil {
			t.Fatal(err)
		}
	}
	g.nodes["y"].Deps = append(g.nodes["y"].Deps, "x")
	g.nodes["z"].Deps = append(g.nodes["z"].Deps, "y")
	g.nodes["x"].Deps = append(g.nodes["x"].Deps, "z")
	_, err := g.Topo()
	var f model.Fault
	if !errors.As(err, &f) || f.Kind != model.FaultGraphCycle {
		t.Fatalf("err = %v, want graph-cycle fault", err)
	}
	if !strings.Contains(f.Reason, "x") || !strings.Contains(f.Reason, "z") {
		t.Fatalf("cycle reason should name blocked nodes, got %q", f.Reason)
	}
}

func TestDuplicateAndUnknownDep(t *testing.T) {
	g := NewGraph()
	if err := g.Add(Node{ID: "a"}); err != nil {
		t.Fatal(err)
	}
	if err := g.Add(Node{ID: "a"}); err == nil {
		t.Fatal("duplicate add must fail")
	}
	if err := g.Add(Node{ID: "b", Deps: []string{"ghost"}}); err == nil {
		t.Fatal("unknown dependency must fail")
	}
}

func TestDownstream(t *testing.T) {
	g := build(t, [][2]string{
		{"src-a", "norm-a"}, {"src-b", "norm-b"},
		{"norm-a", "align"}, {"norm-b", "align"}, {"align", "manifest"},
	})
	got, ok := g.Downstream("src-a")
	if !ok {
		t.Fatal("node missing")
	}
	want := []string{"src-a", "norm-a", "align", "manifest"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("downstream = %v, want %v", got, want)
	}
	// src-b must not appear in src-a's closure.
	for _, id := range got {
		if id == "src-b" || id == "norm-b" {
			t.Fatalf("unrelated node %s leaked into closure", id)
		}
	}
	if _, ok := g.Downstream("nope"); ok {
		t.Fatal("missing node must report ok=false")
	}
}

func TestNodeAndSize(t *testing.T) {
	g := NewGraph()
	if err := g.Add(Node{ID: "n1", Kind: KindManifest}); err != nil {
		t.Fatal(err)
	}
	if g.Size() != 1 {
		t.Fatalf("size = %d, want 1", g.Size())
	}
	n, ok := g.Node("n1")
	if !ok || n.Kind != KindManifest {
		t.Fatalf("lookup = %+v, %v", n, ok)
	}
}
