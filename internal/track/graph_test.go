package track

import (
	"context"
	"errors"
	"testing"

	"avpipe/internal/clock"
	"avpipe/internal/fault"
)

func newGraph(t *testing.T, nodes ...Node) *Graph {
	t.Helper()
	g := New()
	for _, n := range nodes {
		if err := g.Add(n); err != nil {
			t.Fatalf("add %s: %v", n.Name, err)
		}
	}
	return g
}

func TestCompileOrdersDependenciesFirst(t *testing.T) {
	g := newGraph(t,
		Node{Name: "video", Kind: KindVideo, Deps: []string{"audio"}},
		Node{Name: "audio", Kind: KindAudio},
		Node{Name: "report", Kind: KindDerived, Deps: []string{"video", "audio"}},
	)
	order, err := g.Compile()
	if err != nil {
		t.Fatal(err)
	}
	pos := map[string]int{}
	for i, n := range order {
		pos[n] = i
	}
	if pos["audio"] >= pos["video"] || pos["video"] >= pos["report"] {
		t.Fatalf("bad order %v", order)
	}
}

func TestCompileDeterministicTieBreak(t *testing.T) {
	g := newGraph(t,
		Node{Name: "c", Kind: KindAudio},
		Node{Name: "a", Kind: KindAudio},
		Node{Name: "b", Kind: KindAudio},
	)
	for i := 0; i < 20; i++ {
		order, err := g.Compile()
		if err != nil {
			t.Fatal(err)
		}
		if order[0] != "a" || order[1] != "b" || order[2] != "c" {
			t.Fatalf("order not sorted: %v", order)
		}
	}
}

func TestDuplicateNodeRejected(t *testing.T) {
	g := newGraph(t, Node{Name: "audio", Kind: KindAudio})
	err := g.Add(Node{Name: "audio", Kind: KindVideo})
	e := fault.Root(err)
	if e == nil || e.Code != fault.CodeGraphDuplicate || e.Track != "audio" {
		t.Fatalf("want duplicate error, got %v", err)
	}
}

func TestMissingDependencyLocatable(t *testing.T) {
	g := newGraph(t, Node{Name: "video", Kind: KindVideo, Deps: []string{"ghost"}})
	_, err := g.Compile()
	e := fault.Root(err)
	if e == nil || e.Code != fault.CodeGraphMissing || e.Track != "video" {
		t.Fatalf("want missing dependency, got %v", err)
	}
}

func TestCycleDetectedWithPath(t *testing.T) {
	g := newGraph(t,
		Node{Name: "a", Kind: KindAudio, Deps: []string{"c"}},
		Node{Name: "b", Kind: KindDerived, Deps: []string{"a"}},
		Node{Name: "c", Kind: KindDerived, Deps: []string{"b"}},
	)
	_, err := g.Compile()
	e := fault.Root(err)
	if e == nil || e.Code != fault.CodeGraphCycle {
		t.Fatalf("want cycle, got %v", err)
	}
}

func TestSelfDependencyIsCycle(t *testing.T) {
	g := newGraph(t, Node{Name: "a", Kind: KindAudio, Deps: []string{"a"}})
	if _, err := g.Compile(); err == nil {
		t.Fatal("self dependency accepted")
	}
}

func TestNonMonotonicSpansRejected(t *testing.T) {
	g := newGraph(t, Node{Name: "video", Kind: KindVideo, Spans: []clock.Span{
		{Start: 0, End: 100}, {Start: 50, End: 200},
	}})
	_, err := g.Compile()
	e := fault.Root(err)
	if e == nil || e.Code != fault.CodeGraphMonotonic || e.Track != "video" {
		t.Fatalf("want monotonic error, got %v", err)
	}
}

func TestRunPropagatesCancelAndKeepsRootCause(t *testing.T) {
	g := newGraph(t,
		Node{Name: "audio", Kind: KindAudio},
		Node{Name: "video", Kind: KindVideo, Deps: []string{"audio"}},
		Node{Name: "mux", Kind: KindDerived, Deps: []string{"audio", "video"}},
	)
	bad := fault.New(fault.CodeWAVLength, "pipeline.audio", "bad block").At(99)
	ran := map[string]bool{}
	out, err := g.Run(context.Background(), func(ctx context.Context, name string) error {
		ran[name] = true
		if name == "audio" {
			return bad
		}
		return nil
	})
	if err == nil {
		t.Fatal("run reported success")
	}
	if got := fault.Root(err); got.Code != fault.CodeWAVLength || got.Offset != 99 {
		t.Fatalf("root cause lost: %+v", got)
	}
	if out.Status["video"] != StatusCanceled || out.Status["mux"] != StatusCanceled {
		t.Fatalf("dependents not canceled: %v", out.Status)
	}
	if ran["video"] || ran["mux"] {
		t.Fatalf("canceled nodes executed: %v", ran)
	}
	if got := fault.Root(out.Failures["mux"]); got.Code != fault.CodeWAVLength {
		t.Fatalf("mux failure chain lost root: %+v", got)
	}
}

func TestRunHonorsContextCancellation(t *testing.T) {
	g := newGraph(t, Node{Name: "audio", Kind: KindAudio})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out, err := g.Run(ctx, func(ctx context.Context, name string) error { return nil })
	if err == nil {
		t.Fatal("canceled run reported success")
	}
	if out.Status["audio"] != StatusCanceled {
		t.Fatalf("status=%v", out.Status)
	}
}

func TestRunIndependentNodesContinueAfterFailure(t *testing.T) {
	g := newGraph(t,
		Node{Name: "a", Kind: KindAudio},
		Node{Name: "b", Kind: KindAudio},
	)
	out, err := g.Run(context.Background(), func(ctx context.Context, name string) error {
		if name == "a" {
			return errors.New("boom")
		}
		return nil
	})
	if err == nil || out.Status["b"] != StatusOK {
		t.Fatalf("independent node stopped: %v %v", err, out.Status)
	}
}
