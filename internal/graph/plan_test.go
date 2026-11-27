package graph

import "testing"

func TestBuildOrdersDependencies(t *testing.T) {
	p, err := Build([]string{"v1"}, []string{"a1", "a2"})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(p.Layers) != 3 {
		t.Fatalf("layers: %+v", p.Layers)
	}
	if p.Layers[0][0] != "v1" || p.Layers[1][0] != "a1" || p.Layers[2][0] != "index" {
		t.Fatalf("layer order: %+v", p.Layers)
	}
	deps := p.Nodes["index"].Deps
	if len(deps) != 3 {
		t.Fatalf("index deps: %+v", deps)
	}
	for _, a := range []string{"a1", "a2"} {
		if len(p.Nodes[a].Deps) != 1 || p.Nodes[a].Deps[0] != "v1" {
			t.Fatalf("%s deps: %+v", a, p.Nodes[a].Deps)
		}
	}
}

func TestBuildRejectsDuplicates(t *testing.T) {
	if _, err := Build([]string{"v1"}, []string{"v1"}); err == nil {
		t.Fatal("duplicate id accepted")
	}
	if _, err := Build(nil, []string{"a1"}); err != nil {
		t.Fatalf("audio-only plan should build: %v", err)
	}
}

func TestPlanValidateDetectsOrderInversion(t *testing.T) {
	p, err := Build([]string{"v1"}, []string{"a1"})
	if err != nil {
		t.Fatal(err)
	}
	p.Order = []string{"index", "a1", "v1"}
	if err := p.Validate(); err == nil {
		t.Fatal("inverted order accepted")
	}
}
