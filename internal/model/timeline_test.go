package model

import (
	"errors"
	"testing"
)

func TestClockConversion(t *testing.T) {
	c := Clock{Timescale: 48000}
	if got := c.ToMillis(48000); got != 1000 {
		t.Fatalf("ToMillis: got %d want 1000", got)
	}
	if got := c.FromMillis(40); got != 1920 {
		t.Fatalf("FromMillis: got %d want 1920", got)
	}
}

func TestMediaErrorContext(t *testing.T) {
	e := &MediaError{Code: ErrGap, Module: "recovery", Track: "v1",
		Seq: 18, Timestamp: 540000, Msg: "burst loss not covered"}
	w := e.With("sync", "cannot align across gap")
	if w.Code != ErrGap || w.Track != "v1" || w.Seq != 18 {
		t.Fatalf("context lost: %+v", w)
	}
	if !errors.Is(w, e.Cause) && w.Unwrap() != e.Cause {
		t.Fatalf("unwrap chain broken")
	}
	if ExitCode(e) != ExitUnrecoverable {
		t.Fatalf("exit code: got %d", ExitCode(e))
	}
	if ExitCode(errors.New("x")) != ExitInternal {
		t.Fatalf("plain error must map to internal exit")
	}
}

func TestTopoSortOrder(t *testing.T) {
	g := &Graph{Nodes: []Node{
		{ID: "sync", Deps: []string{"recovery"}},
		{ID: "manifest"},
		{ID: "recovery", Deps: []string{"reorder"}},
		{ID: "reorder", Deps: []string{"manifest"}},
	}}
	order, err := g.TopoSort()
	if err != nil {
		t.Fatal(err)
	}
	pos := map[string]int{}
	for i, id := range order {
		pos[id] = i
	}
	for _, n := range g.Nodes {
		for _, d := range n.Deps {
			if pos[d] > pos[n.ID] {
				t.Fatalf("%s before its dep %s", n.ID, d)
			}
		}
	}
}

func TestTopoSortMissingDep(t *testing.T) {
	g := &Graph{Nodes: []Node{{ID: "a", Deps: []string{"ghost"}}}}
	_, err := g.TopoSort()
	me, ok := err.(*MediaError)
	if !ok || me.Code != ErrGraph {
		t.Fatalf("want ErrGraph, got %v", err)
	}
	if me.Msg == "" || !contains(me.Msg, "ghost") {
		t.Fatalf("error must name missing node: %s", me.Msg)
	}
}

func TestTopoSortCycle(t *testing.T) {
	g := &Graph{Nodes: []Node{
		{ID: "a", Deps: []string{"b"}},
		{ID: "b", Deps: []string{"a"}},
	}}
	_, err := g.TopoSort()
	me, ok := err.(*MediaError)
	if !ok || me.Code != ErrGraph || !contains(me.Msg, "cycle") {
		t.Fatalf("want cycle ErrGraph, got %v", err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
