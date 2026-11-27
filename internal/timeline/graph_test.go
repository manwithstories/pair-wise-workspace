package timeline

import (
	"errors"
	"math"
	"testing"

	"fieldcut/internal/media"
)

func offsetMs(ms float64) media.Rational {
	return media.NewRational(int64(math.Round(ms*1000)), 1e6)
}

func TestBuildTopologicalOrder(t *testing.T) {
	g, err := Build([]Node{
		{Name: "bg", Priority: 10},
		{Name: "mid", Priority: 50, Deps: []string{"bg"}},
		{Name: "fg", Priority: 90, Deps: []string{"mid"}, Offset: offsetMs(40), Mask: Rect{X: 100, Y: 100, W: 320, H: 180}},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	order := g.Order()
	if len(order) != 3 || order[0] != "bg" || order[2] != "fg" {
		t.Fatalf("order = %v", order)
	}
	fg, _ := g.Node("fg")
	if fg.Offset.Cmp(offsetMs(40)) != 0 {
		t.Fatalf("offset = %v, want equivalent of 40/1000", fg.Offset)
	}
}

func TestBuildDetectsCycleBeforeOutput(t *testing.T) {
	_, err := Build([]Node{
		{Name: "a", Deps: []string{"c"}},
		{Name: "b", Deps: []string{"a"}},
		{Name: "c", Deps: []string{"b"}},
	})
	if err == nil {
		t.Fatal("cycle accepted")
	}
	var me *media.MediaError
	if !errors.As(err, &me) || me.Kind != media.KindGraph {
		t.Fatalf("not a graph error: %v", err)
	}
}

func TestBuildRejectsUnknownAndSelfDeps(t *testing.T) {
	if _, err := Build([]Node{{Name: "a", Deps: []string{"ghost"}}}); err == nil {
		t.Fatal("unknown dep accepted")
	}
	if _, err := Build([]Node{{Name: "a", Deps: []string{"a"}}}); err == nil {
		t.Fatal("self dep accepted")
	}
}

func TestCompositeOrderAscendsPriority(t *testing.T) {
	g, err := Build([]Node{
		{Name: "fg", Priority: 90},
		{Name: "bg", Priority: 0},
		{Name: "mid", Priority: 40},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := g.CompositeOrder()
	if got[0] != "bg" || got[1] != "mid" || got[2] != "fg" {
		t.Fatalf("composite order = %v", got)
	}
}

func TestMaskIntersect(t *testing.T) {
	a := Rect{X: 0, Y: 0, W: 100, H: 100}
	b := Rect{X: 50, Y: 50, W: 100, H: 100}
	got := a.Intersect(b)
	if got != (Rect{X: 50, Y: 50, W: 50, H: 50}) {
		t.Fatalf("intersect = %+v", got)
	}
	if !a.Intersect(Rect{X: 200, Y: 200, W: 10, H: 10}).Empty() {
		t.Fatal("disjoint rects must be empty")
	}
}

func TestBuildRejectsLimitAndBadPriority(t *testing.T) {
	var nodes []Node
	for i := 0; i < media.MaxTracks+1; i++ {
		nodes = append(nodes, Node{Name: string(rune('a' + i))})
	}
	if _, err := Build(nodes); err == nil {
		t.Fatal("too many tracks accepted")
	}
	if _, err := Build([]Node{{Name: "a", Priority: 101}}); err == nil {
		t.Fatal("priority out of range accepted")
	}
}
