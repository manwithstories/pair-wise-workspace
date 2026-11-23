package scheduler

import (
	"context"
	"errors"
	"testing"
)

func node(name string, deps ...string) *Node {
	return &Node{Name: name, Deps: deps, Version: "v1", Reentrant: true,
		Run: func(ctx context.Context, deps map[string]any) (any, error) {
			return name + "-out", nil
		}}
}

func TestRunTopoOrder(t *testing.T) {
	s := New()
	var ran []string
	mk := func(name string, deps ...string) *Node {
		n := node(name, deps...)
		n.Run = func(ctx context.Context, d map[string]any) (any, error) {
			ran = append(ran, name)
			return name, nil
		}
		return n
	}
	s.Add(mk("c", "a", "b"))
	s.Add(mk("a"))
	s.Add(mk("b", "a"))
	if err := s.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(ran) != 3 || ran[0] != "a" || ran[2] != "c" {
		t.Fatalf("order=%v", ran)
	}
	if s.Results()["c"] != "c" {
		t.Fatal("results missing")
	}
}

func TestBlockedReportsFirstNodeAndChain(t *testing.T) {
	s := New()
	s.Add(node("a"))
	bad := node("b", "a")
	bad.Reentrant = false
	bad.Run = func(ctx context.Context, d map[string]any) (any, error) {
		return nil, errors.New("corrupt index segment")
	}
	s.Add(bad)
	s.Add(node("c", "b"))
	s.Add(node("d", "c"))
	err := s.Run(context.Background())
	be, ok := err.(*BlockedError)
	if !ok {
		t.Fatalf("want *BlockedError, got %T (%v)", err, err)
	}
	if be.Node != "b" {
		t.Fatalf("blocked node=%s want b", be.Node)
	}
	if len(be.Chain) != 3 || be.Chain[0] != "b" || be.Chain[2] != "d" {
		t.Fatalf("chain=%v", be.Chain)
	}
	// a succeeded and must be reusable on resume.
	if s.Results()["a"] != "a-out" {
		t.Fatal("successful node result not retained")
	}
}

func TestResumeReusesSucceededNodes(t *testing.T) {
	s := New()
	runs := map[string]int{}
	mk := func(name string, fail bool, deps ...string) *Node {
		n := node(name, deps...)
		n.Run = func(ctx context.Context, d map[string]any) (any, error) {
			runs[name]++
			if fail {
				return nil, errors.New("boom")
			}
			return name, nil
		}
		return n
	}
	a := mk("a", false)
	b := mk("b", true, "a")
	b.Reentrant = false
	s.Add(a)
	s.Add(b)
	if err := s.Run(context.Background()); err == nil {
		t.Fatal("expected failure")
	}
	// Fix b and resume: a must not re-run.
	b.Run = func(ctx context.Context, d map[string]any) (any, error) {
		runs["b"]++
		return "b-fixed", nil
	}
	if err := s.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runs["a"] != 1 {
		t.Fatalf("a re-ran %d times", runs["a"])
	}
	if s.Results()["b"] != "b-fixed" {
		t.Fatal("fixed node result missing")
	}
}

func TestRetryOnlyReentrant(t *testing.T) {
	ctx := context.Background()
	// Reentrant node: retried until success.
	s := New()
	tries := 0
	n := node("flaky")
	n.Run = func(ctx context.Context, d map[string]any) (any, error) {
		tries++
		if tries < 3 {
			return nil, errors.New("transient")
		}
		return "ok", nil
	}
	s.Add(n)
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if tries != 3 {
		t.Fatalf("tries=%d want 3", tries)
	}
	// Non-reentrant node: exactly one attempt.
	s2 := New()
	tries2 := 0
	n2 := node("fragile")
	n2.Reentrant = false
	n2.Run = func(ctx context.Context, d map[string]any) (any, error) {
		tries2++
		return nil, errors.New("always")
	}
	s2.Add(n2)
	if err := s2.Run(ctx); err == nil {
		t.Fatal("expected failure")
	}
	if tries2 != 1 {
		t.Fatalf("non-reentrant tried %d times", tries2)
	}
}

func TestIdempotencyKeyStable(t *testing.T) {
	n := node("x")
	n.Params = func() string { return "p=1" }
	k1 := idempotencyKey(n, []string{"da", "db"})
	k2 := idempotencyKey(n, []string{"da", "db"})
	if k1 != k2 {
		t.Fatal("key not stable")
	}
	n.Version = "v2"
	if idempotencyKey(n, []string{"da", "db"}) == k1 {
		t.Fatal("version bump must change key")
	}
}

func TestCycleAndUnknownDep(t *testing.T) {
	s := New()
	s.Add(node("a", "b"))
	s.Add(node("b", "a"))
	if err := s.Run(context.Background()); err == nil {
		t.Fatal("cycle accepted")
	}
	s2 := New()
	s2.Add(node("a", "ghost"))
	if err := s2.Run(context.Background()); err == nil {
		t.Fatal("unknown dep accepted")
	}
}
