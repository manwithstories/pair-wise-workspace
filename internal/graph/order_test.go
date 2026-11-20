package graph

import (
	"testing"

	"github.com/mediamend/mediamend/internal/model"
)

func TestOrderLayers(t *testing.T) {
	g := New()
	mustAdd(t, g, "a1", KindAudio)
	mustAdd(t, g, "v1", KindVideo)
	mustAdd(t, g, "idx1", KindIndex)
	if err := g.AddDependency("v1", "a1"); err != nil { // video waits on audio
		t.Fatal(err)
	}
	if err := g.AddDependency("idx1", "v1"); err != nil { // index waits on repair
		t.Fatal(err)
	}
	layers, cycle, faults, err := g.Order("project.json")
	if err != nil || cycle != nil || faults != nil {
		t.Fatalf("unexpected failure: %v faults=%v", err, faults)
	}
	want := [][]string{{"a1"}, {"v1"}, {"idx1"}}
	if len(layers) != len(want) {
		t.Fatalf("layers=%v want %v", layers, want)
	}
	for i := range want {
		if len(layers[i]) != len(want[i]) || layers[i][0] != want[i][0] {
			t.Fatalf("layer %d = %v want %v", i, layers[i], want[i])
		}
	}
}

func TestOrderCycle(t *testing.T) {
	g := New()
	mustAdd(t, g, "a", KindAudio)
	mustAdd(t, g, "b", KindVideo)
	mustAdd(t, g, "c", KindIndex)
	_ = g.AddDependency("a", "b")
	_ = g.AddDependency("b", "c")
	_ = g.AddDependency("c", "a")
	layers, cycle, faults, err := g.Order("project.json")
	if err == nil || layers != nil {
		t.Fatal("cycle not reported")
	}
	if len(faults) != 1 || faults[0].Class != model.FaultGraphCycle || faults[0].Recoverable() {
		t.Fatalf("faults=%v", faults)
	}
	if len(cycle) < 3 || cycle[0] != cycle[len(cycle)-1] {
		t.Fatalf("cycle path=%v", cycle)
	}
}

func TestBlockedByTransitive(t *testing.T) {
	g := New()
	mustAdd(t, g, "a", KindAudio)
	mustAdd(t, g, "v", KindVideo)
	mustAdd(t, g, "idx", KindIndex)
	mustAdd(t, g, "other", KindAudio)
	_ = g.AddDependency("v", "a")
	_ = g.AddDependency("idx", "v")
	blocked := g.BlockedBy([]string{"a"})
	if len(blocked) != 2 || blocked[0] != "idx" || blocked[1] != "v" {
		t.Fatalf("blocked=%v want [idx v]", blocked)
	}
}

func TestUnknownDependencyRejected(t *testing.T) {
	g := New()
	_ = g.AddTrack("a", KindAudio)
	if err := g.AddDependency("a", "ghost"); err == nil {
		t.Fatal("dangling dependency accepted")
	}
}

func mustAdd(t *testing.T, g *Graph, id string, k Kind) {
	t.Helper()
	if err := g.AddTrack(id, k); err != nil {
		t.Fatal(err)
	}
}
