package dag

import (
	"testing"
)

func noop() func(*RunContext) error { return func(*RunContext) error { return nil } }

func TestNewGraph_TopologicalOrderStable(t *testing.T) {
	g, err := NewGraph([]Node{
		{ID: "c", Inputs: []string{"b"}, InputDigest: "d-c", Run: noop()},
		{ID: "a", InputDigest: "d-a", Run: noop()},
		{ID: "b", Inputs: []string{"a"}, InputDigest: "d-b", Run: noop()},
	})
	if err != nil {
		t.Fatalf("NewGraph: %v", err)
	}
	order := g.Order()
	if len(order) != 3 || order[0] != "a" || order[1] != "b" || order[2] != "c" {
		t.Fatalf("topological order = %v", order)
	}
	edges := g.Edges()
	if len(edges) != 2 || edges[0].From != "a" || edges[1].From != "b" {
		t.Fatalf("edges = %+v", edges)
	}
}

func TestNewGraph_ValidationErrors(t *testing.T) {
	if _, err := NewGraph([]Node{{ID: "", Run: noop()}}); err == nil {
		t.Fatal("empty id must fail")
	}
	if _, err := NewGraph([]Node{
		{ID: "x", Run: noop()}, {ID: "x", Run: noop()},
	}); err == nil {
		t.Fatal("duplicate id must fail")
	}
	if _, err := NewGraph([]Node{
		{ID: "x", Inputs: []string{"ghost"}, Run: noop()},
	}); err == nil {
		t.Fatal("unknown input must fail")
	}
	if _, err := NewGraph([]Node{
		{ID: "x", Inputs: []string{"y"}, Run: noop()},
		{ID: "y", Inputs: []string{"x"}, Run: noop()},
	}); err == nil {
		t.Fatal("cycle must fail")
	}
	if _, err := NewGraph([]Node{
		{ID: "x", Inputs: []string{"y", "y"}, Run: noop()},
		{ID: "y", Run: noop()},
	}); err == nil {
		t.Fatal("duplicate edge must fail")
	}
}

func TestIdempotentKey_ChangesWithDigestAndUpstream(t *testing.T) {
	g, err := NewGraph([]Node{
		{ID: "a", InputDigest: "digest-a", Run: noop()},
		{ID: "b", Inputs: []string{"a"}, InputDigest: "digest-b", Run: noop()},
	})
	if err != nil {
		t.Fatal(err)
	}
	keys1, err := g.AllKeys()
	if err != nil {
		t.Fatal(err)
	}
	if keys1["a"].Value == "" || keys1["b"].Value == "" {
		t.Fatal("keys must be populated")
	}

	// Same graph rebuilt -> identical keys (reproducibility).
	g2, _ := NewGraph([]Node{
		{ID: "a", InputDigest: "digest-a", Run: noop()},
		{ID: "b", Inputs: []string{"a"}, InputDigest: "digest-b", Run: noop()},
	})
	keys2, _ := g2.AllKeys()
	if keys1["a"].Value != keys2["a"].Value || keys1["b"].Value != keys2["b"].Value {
		t.Fatal("equal inputs must yield equal idempotent keys")
	}

	// Changed upstream digest invalidates the dependent key too.
	g3, _ := NewGraph([]Node{
		{ID: "a", InputDigest: "digest-A-CHANGED", Run: noop()},
		{ID: "b", Inputs: []string{"a"}, InputDigest: "digest-b", Run: noop()},
	})
	keys3, _ := g3.AllKeys()
	if keys3["a"].Value == keys1["a"].Value {
		t.Fatal("changed own digest must change the key")
	}
	if keys3["b"].Value == keys1["b"].Value {
		t.Fatal("upstream digest change must invalidate the dependent key")
	}
}

func TestFirstBlocker_TopologicalFirst(t *testing.T) {
	g, _ := NewGraph([]Node{
		{ID: "root", InputDigest: "r", Run: noop()},
		{ID: "mid", Inputs: []string{"root"}, InputDigest: "m", Run: noop()},
		{ID: "leaf", Inputs: []string{"mid"}, InputDigest: "l", Run: noop()},
		{ID: "other", Inputs: []string{"root"}, InputDigest: "o", Run: noop()},
	})
	status := map[string]Status{
		"root":  StatusSuccess,
		"mid":   StatusFailed,
		"leaf":  StatusBlocked,
		"other": StatusSuccess,
	}
	blocker, err := g.FirstBlocker(status, "leaf")
	if err != nil {
		t.Fatal(err)
	}
	if blocker != "mid" {
		t.Fatalf("blocker = %q, want the first failed ancestor mid", blocker)
	}
	// An unrelated healthy branch must report no blocker.
	if b, _ := g.FirstBlocker(status, "other"); b != "" {
		t.Fatalf("unrelated node must not be blocked, got %q", b)
	}
}
