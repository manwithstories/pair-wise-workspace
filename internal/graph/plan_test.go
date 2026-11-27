package graph

import (
	"strings"
	"testing"

	"avpipe/internal/media"
)

func nodes(ids ...string) []Node {
	var out []Node
	for _, id := range ids {
		out = append(out, Node{ID: id, Stage: StageSource})
	}
	return out
}

func TestStableTopologicalOrder(t *testing.T) {
	ns := nodes("sink", "resample", "source", "audit")
	for i := range ns {
		switch ns[i].ID {
		case "resample":
			ns[i].Deps = []string{"source"}
		case "sink":
			ns[i].Deps = []string{"resample"}
		case "audit":
			ns[i].Deps = []string{"sink", "source"}
		}
	}
	p, err := Build(ns, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(p.Order(), ",")
	want := "source,resample,sink,audit"
	if got != want {
		t.Fatalf("order=%s want %s", got, want)
	}
	// Rebuilding from a shuffled copy must produce the same order.
	shuffled := []Node{ns[2], ns[0], ns[3], ns[1]}
	p2, err := Build(shuffled, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(p2.Order(), ",") != want {
		t.Fatalf("order not stable: %v", p2.Order())
	}
}

func TestCycleRejected(t *testing.T) {
	ns := []Node{
		{ID: "a", Stage: StageSource, Deps: []string{"c"}},
		{ID: "b", Stage: StageResample, Deps: []string{"a"}},
		{ID: "c", Stage: StageSink, Deps: []string{"b"}},
	}
	_, err := Build(ns, nil, 0)
	if err == nil {
		t.Fatal("cycle accepted")
	}
	if media.ExitFor(err) != media.ExitUsage {
		t.Fatalf("exit=%d", media.ExitFor(err))
	}
	pe, _ := media.AsError(err)
	if pe.Phase != media.PhaseGraph || !strings.HasPrefix(pe.Err.Error(), "dependency cycle") {
		t.Fatalf("err=%v", err)
	}
}

func TestMissingTrackAndNode(t *testing.T) {
	_, err := Build([]Node{{ID: "a", Stage: StageSource, Track: "ghost"}}, []string{"mix"}, 0)
	if err == nil || media.ExitFor(err) != media.ExitUsage {
		t.Fatalf("missing track: %v", err)
	}
	_, err = Build([]Node{{ID: "a", Stage: StageSource, Deps: []string{"nope"}}}, nil, 0)
	if err == nil || media.ExitFor(err) != media.ExitUsage {
		t.Fatalf("missing node: %v", err)
	}
}

func TestAnchorSpanBounds(t *testing.T) {
	ok := []Node{{ID: "audit", Stage: StageAudit, Anchors: &Span{Lo: 0, Hi: 3}}}
	if _, err := Build(ok, nil, 3); err != nil {
		t.Fatal(err)
	}
	bad := []Node{{ID: "audit", Stage: StageAudit, Anchors: &Span{Lo: 0, Hi: 4}}}
	if _, err := Build(bad, nil, 3); err == nil {
		t.Fatal("out-of-range span accepted")
	}
	neg := []Node{{ID: "audit", Stage: StageAudit, Anchors: &Span{Lo: -1, Hi: 2}}}
	if _, err := Build(neg, nil, 3); err == nil {
		t.Fatal("negative span accepted")
	}
}

func TestDuplicateAndSelfDependency(t *testing.T) {
	if _, err := Build([]Node{{ID: "a", Stage: StageSource}, {ID: "a", Stage: StageSink}}, nil, 0); err == nil {
		t.Fatal("duplicate id accepted")
	}
	if _, err := Build([]Node{{ID: "a", Stage: StageSource, Deps: []string{"a"}}}, nil, 0); err == nil {
		t.Fatal("self dependency accepted")
	}
	if _, err := Build(nil, nil, 0); err == nil {
		t.Fatal("empty plan accepted")
	}
}

func TestNodeLookup(t *testing.T) {
	p, err := Build([]Node{{ID: "a", Stage: StageSource, Track: "m"}}, []string{"m"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if p.Len() != 1 {
		t.Fatalf("len=%d", p.Len())
	}
	n, ok := p.Node("a")
	if !ok || n.Track != "m" {
		t.Fatalf("node=%+v ok=%v", n, ok)
	}
	if _, ok := p.Node("z"); ok {
		t.Fatal("unknown node found")
	}
	if len(p.Order()) != 1 {
		t.Fatalf("order=%v", p.Order())
	}
}
