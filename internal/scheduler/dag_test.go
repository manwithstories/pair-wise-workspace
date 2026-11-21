package scheduler

import (
	"errors"
	"testing"

	"mediad/internal/model"
)

func okNode(name string, deps ...string) Node {
	return Node{Name: name, Deps: deps, Run: func() (Result, error) {
		return Result{Hash: "hash:" + name}, nil
	}}
}

func failingNode(name string, err error) Node {
	return Node{Name: name, Run: func() (Result, error) { return Result{}, err }}
}

func TestDeterministicTopologicalOrder(t *testing.T) {
	// b and a both ready; ordering must not depend on map iteration.
	nodes := []Node{
		okNode("render", "compose"),
		okNode("compose", "metadata", "transport"),
		okNode("transport"),
		okNode("metadata"),
		okNode("manifest", "render"),
	}
	s, err := New(nodes)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"metadata", "transport", "compose", "render", "manifest"}
	got := s.Order()
	if len(got) != len(want) {
		t.Fatalf("order = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
	// Rebuilding the graph yields the same order.
	s2, _ := New(nodes)
	if s2.Order()[0] != got[0] || s2.Order()[4] != got[4] {
		t.Fatal("order not stable across builds")
	}
}

func TestCycleRejected(t *testing.T) {
	_, err := New([]Node{okNode("a", "b"), okNode("b", "a")})
	if err == nil {
		t.Fatal("cycle accepted")
	}
}

func TestUnknownDependencyRejected(t *testing.T) {
	_, err := New([]Node{okNode("a", "ghost")})
	if err == nil {
		t.Fatal("unknown dependency accepted")
	}
	if model.FirstBlocking(err) != "ghost" {
		t.Fatalf("blocking = %q", model.FirstBlocking(err))
	}
}

func TestCleanRun(t *testing.T) {
	s, _ := New([]Node{okNode("a"), okNode("b", "a"), okNode("c", "a")})
	r := s.Run()
	if r.Succeeded != 3 || r.Failed != 0 || r.Blocked != 0 {
		t.Fatalf("report = %+v", r)
	}
	if r.FirstBlockOrNone() != "none" {
		t.Fatalf("first block = %s", r.FirstBlockOrNone())
	}
}

func TestFailureBlocksDependentsButNotSiblings(t *testing.T) {
	boom := errors.New("corrupt metadata")
	s, _ := New([]Node{
		failingNode("metadata", boom),
		okNode("transport"),
		okNode("compose", "metadata", "transport"),
		okNode("render", "compose"),
		okNode("manifest", "render"),
		okNode("audit", "transport"), // independent branch
	})
	r := s.Run()
	if r.States["metadata"].Status != StatusFailed {
		t.Fatal("metadata should fail")
	}
	for _, name := range []string{"compose", "render", "manifest"} {
		if r.States[name].Status != StatusBlocked {
			t.Fatalf("%s = %s, want blocked", name, r.States[name].Status)
		}
	}
	if r.States["audit"].Status != StatusDone {
		t.Fatal("independent branch was blocked")
	}
	if r.FirstBlock != "metadata" {
		t.Fatalf("first block = %q", r.FirstBlock)
	}
	// The retained blocking dependency is the root cause, not the neighbour.
	if r.States["render"].BlockedBy != "compose" || r.States["render"].RootCause != "metadata" {
		t.Fatalf("render state = %+v", r.States["render"])
	}
}

func TestFirstBlockingDependencyRetained(t *testing.T) {
	s, _ := New([]Node{
		failingNode("a", errors.New("first")),
		failingNode("b", errors.New("second")),
		okNode("c", "a", "b"),
	})
	r := s.Run()
	// Both fail; c must retain the first failed dependency in order ("a").
	if r.States["c"].BlockedBy != "a" {
		t.Fatalf("blocked by %q, want a", r.States["c"].BlockedBy)
	}
	if r.FirstBlock != "a" {
		t.Fatalf("first block %q", r.FirstBlock)
	}
}

func TestRetrySucceedsAfterTransientFailure(t *testing.T) {
	attempts := 0
	s, _ := New([]Node{{Name: "flaky", Run: func() (Result, error) {
		attempts++
		if attempts < 3 {
			return Result{}, errors.New("transient")
		}
		return Result{Hash: "ok"}, nil
	}}})
	s.MaxAttempts = 3
	r := s.Run()
	if r.States["flaky"].Status != StatusDone || r.States["flaky"].Attempts != 3 {
		t.Fatalf("state = %+v", r.States["flaky"])
	}
	if len(r.FailedNodes()) != 0 {
		t.Fatal("unexpected failure")
	}
}

func TestRetryExhaustsAndReports(t *testing.T) {
	attempts := 0
	s, _ := New([]Node{{Name: "dead", Run: func() (Result, error) {
		attempts++
		return Result{}, errors.New("permanent")
	}}})
	s.MaxAttempts = 2
	r := s.Run()
	if attempts != 2 || r.States["dead"].Status != StatusFailed || r.States["dead"].Attempts != 2 {
		t.Fatalf("attempts=%d state=%+v", attempts, r.States["dead"])
	}
}

func TestRetryIsDependencyOrderedAndIdempotent(t *testing.T) {
	var order []string
	fail := 0
	s, _ := New([]Node{
		{Name: "a", Run: func() (Result, error) {
			order = append(order, "a")
			return Result{Hash: "A"}, nil
		}},
		{Name: "b", Deps: []string{"a"}, Run: func() (Result, error) {
			order = append(order, "b")
			if fail == 0 {
				fail++
				return Result{}, errors.New("once")
			}
			return Result{Hash: "B"}, nil
		}},
	})
	s.MaxAttempts = 2
	r := s.Run()
	if r.States["b"].Status != StatusDone || r.States["b"].Attempts != 2 {
		t.Fatalf("b = %+v", r.States["b"])
	}
	// a ran exactly once (its product is idempotent); b ran twice.
	want := []string{"a", "b", "b"}
	if len(order) != len(want) {
		t.Fatalf("order = %v", order)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}

func TestIdempotencyKeyDependsOnDepHashes(t *testing.T) {
	a := IdempotencyKey("render", []string{"x", "y"})
	if a != IdempotencyKey("render", []string{"x", "y"}) {
		t.Fatal("key not stable")
	}
	if a == IdempotencyKey("render", []string{"y", "x"}) {
		t.Fatal("key ignores dependency order")
	}
	if a == IdempotencyKey("manifest", []string{"x", "y"}) {
		t.Fatal("key ignores node name")
	}
}

func TestIdempotentHelper(t *testing.T) {
	n := 0
	attempts, err := Idempotent(3, func() error {
		n++
		if n < 2 {
			return errors.New("once")
		}
		return nil
	})
	if err != nil || attempts != 2 {
		t.Fatalf("attempts=%d err=%v", attempts, err)
	}
	if _, err := Idempotent(0, func() error { return errors.New("x") }); err == nil {
		t.Fatal("expected error")
	}
}

func TestCategorizedErrorPreserved(t *testing.T) {
	boom := model.New(model.CategoryMetadata, "parse", "bad colr").WithTrack("v1")
	s, _ := New([]Node{{Name: "metadata", Run: func() (Result, error) { return Result{}, boom }}})
	r := s.Run()
	st := r.States["metadata"]
	if st.Err == nil || st.Err.Category != model.CategoryMetadata || st.Err.Track != "v1" {
		t.Fatalf("error detail lost: %+v", st.Err)
	}
	if st.Err.BlockingDep != "metadata" {
		t.Fatalf("blocking dep = %q", st.Err.BlockingDep)
	}
}

func TestResumeSkipsCompletedNodes(t *testing.T) {
	runs := map[string]int{}
	node := func(name string, deps ...string) Node {
		return Node{Name: name, Deps: deps, Run: func() (Result, error) {
			runs[name]++
			return Result{Hash: "hash:" + name}, nil
		}}
	}
	s, _ := New([]Node{node("metadata"), node("transport"), node("compose", "metadata", "transport")})
	// First attempt: metadata finished, then the run was interrupted.
	prior := map[string]NodeState{
		"metadata": {Name: "metadata", Status: StatusDone, Result: Result{Hash: "hash:metadata"}, Attempts: 1},
	}
	r := s.Resume(prior)
	// A completed node is replayed once to rebuild in-memory state; it is not
	// counted as a new attempt and must reproduce its recorded hash.
	if runs["metadata"] != 1 {
		t.Fatalf("completed node replayed %d times", runs["metadata"])
	}
	if r.States["metadata"].Attempts != 1 {
		t.Fatalf("resumed attempts = %d, want 1", r.States["metadata"].Attempts)
	}
	if runs["transport"] != 1 || runs["compose"] != 1 {
		t.Fatalf("pending nodes not run: %+v", runs)
	}
	if r.Succeeded != 3 || r.FirstBlock != "" {
		t.Fatalf("report = %+v", r)
	}
}

func TestResumeRejectsDoneStateWithoutHash(t *testing.T) {
	s, _ := New([]Node{okNode("a")})
	r := s.Resume(map[string]NodeState{"a": {Name: "a", Status: StatusDone}})
	if r.States["a"].Status == StatusDone {
		t.Fatal("resumed a completed node with no product hash")
	}
}
