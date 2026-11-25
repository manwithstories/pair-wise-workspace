package dag

import (
	"errors"
	"testing"

	"mediad/internal/fault"
)

func TestRunTopoOrder(t *testing.T) {
	var ran []string
	mk := func(id string, deps ...string) *Node {
		return &Node{ID: id, Deps: deps, Run: func() error {
			ran = append(ran, id)
			return nil
		}}
	}
	s := New([]*Node{mk("c", "b"), mk("a"), mk("b", "a")}, nil)
	rep, err := s.Run()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a", "b", "c"}
	for i := range want {
		if ran[i] != want[i] {
			t.Fatalf("order %v, want %v", ran, want)
		}
	}
	if len(rep.Completed) != 3 {
		t.Fatalf("completed=%v", rep.Completed)
	}
}

func TestIdempotentSkip(t *testing.T) {
	calls := 0
	n := &Node{ID: "x", Params: "p", Input: []byte("in"), Run: func() error {
		calls++
		return nil
	}}
	s := New([]*Node{n}, nil)
	if _, err := s.Run(); err != nil {
		t.Fatal(err)
	}
	// Second run with the same completed set must not re-execute.
	s2 := New([]*Node{n}, s.Completed)
	rep, err := s2.Run()
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("run executed %d times, want 1 (no duplicate writes)", calls)
	}
	if len(rep.Skipped) != 1 || rep.Skipped[0] != "x" {
		t.Fatalf("skipped=%v", rep.Skipped)
	}
}

func TestRetryThenSuccess(t *testing.T) {
	calls := 0
	n := &Node{ID: "flaky", MaxRetries: 2, Run: func() error {
		calls++
		if calls < 3 {
			return errors.New("transient")
		}
		return nil
	}}
	rep, err := New([]*Node{n}, nil).Run()
	if err != nil {
		t.Fatal(err)
	}
	if rep.Retries != 2 {
		t.Fatalf("retries=%d, want 2", rep.Retries)
	}
}

func TestFirstBlockedInTopoOrder(t *testing.T) {
	boom := errors.New("permanent")
	nodes := []*Node{
		{ID: "fetch:input-4", MaxRetries: 2, Run: func() error { return boom }},
		{ID: "decode:input-4", Deps: []string{"fetch:input-4"}, Run: func() error { return nil }},
		{ID: "mux", Deps: []string{"decode:input-4"}, Run: func() error { return nil }},
	}
	rep, err := New(nodes, nil).Run()
	if err == nil {
		t.Fatal("expected failure")
	}
	if rep.Retries != 2 {
		t.Fatalf("retries=%d, want 2", rep.Retries)
	}
	if rep.FirstBlocked != "decode:input-4" {
		t.Fatalf("first_blocked=%s, want decode:input-4", rep.FirstBlocked)
	}
	if len(rep.BlockedBy) != 1 || rep.BlockedBy[0] != "fetch:input-4" {
		t.Fatalf("blocked_by=%v", rep.BlockedBy)
	}
	if fault.KindOf(err) != fault.KindBlocked {
		t.Fatalf("kind=%v, want blocked", fault.KindOf(err))
	}
	// mux must also be blocked, not executed
	for _, c := range rep.Completed {
		if c == "mux" {
			t.Fatal("mux must not run while blocked")
		}
	}
}

func TestCycleDetected(t *testing.T) {
	mk := func(id string, deps ...string) *Node {
		return &Node{ID: id, Deps: deps, Run: func() error { return nil }}
	}
	_, err := New([]*Node{mk("a", "b"), mk("b", "a")}, nil).Run()
	if fault.KindOf(err) != fault.KindCycle {
		t.Fatalf("want cycle error, got %v", err)
	}
}

func TestKeyDependsOnInputAndParams(t *testing.T) {
	a := &Node{ID: "n", Params: "p1", Input: []byte("x")}
	b := &Node{ID: "n", Params: "p1", Input: []byte("y")}
	c := &Node{ID: "n", Params: "p2", Input: []byte("x")}
	if a.Key() == b.Key() || a.Key() == c.Key() {
		t.Fatal("key must change with input or params")
	}
	if a.Key() != (&Node{ID: "n", Params: "p1", Input: []byte("x")}).Key() {
		t.Fatal("key must be stable for identical input+params")
	}
}
