package schedule

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"mediac/internal/core"
)

func TestTopologicalOrderAndValidation(t *testing.T) {
	nodes := []Node{
		{ID: "report", Deps: []string{"remux", "metrics"}, Run: noopRun},
		{ID: "remux", Deps: []string{"metrics"}, Run: noopRun},
		{ID: "metrics", Deps: []string{"inspect"}, Run: noopRun},
		{ID: "inspect", Deps: []string{"recover"}, Run: noopRun},
		{ID: "recover", Run: noopRun},
	}
	d, err := NewDAG(nodes)
	if err != nil {
		t.Fatal(err)
	}
	order := d.Order()
	pos := map[string]int{}
	for i, id := range order {
		pos[id] = i
	}
	for _, pair := range [][2]string{
		{"recover", "inspect"}, {"inspect", "metrics"},
		{"metrics", "remux"}, {"remux", "report"},
	} {
		if pos[pair[0]] >= pos[pair[1]] {
			t.Fatalf("topo violated: %s must precede %s in %v", pair[0], pair[1], order)
		}
	}
	if _, err := NewDAG([]Node{{ID: "a", Deps: []string{"ghost"}, Run: noopRun}}); err == nil {
		t.Fatal("unknown dependency must be rejected")
	}
	if _, err := NewDAG([]Node{
		{ID: "a", Deps: []string{"b"}, Run: noopRun},
		{ID: "b", Deps: []string{"a"}, Run: noopRun},
	}); err == nil {
		t.Fatal("cycle must be rejected")
	}
	if _, err := NewDAG([]Node{{ID: "a", Deps: []string{"a"}, Run: noopRun}}); err == nil {
		t.Fatal("self cycle must be rejected")
	}
}

func TestConcurrentNoDuplicateWrites(t *testing.T) {
	var mu sync.Mutex
	writes := map[string]int{}
	nodes := makeNodesWithWrites(6, &mu, writes)
	d, _ := NewDAG(nodes)
	rec := core.NewRecorder()
	exec := NewExecutor(Config{Workers: 4, MaxRetry: 0}, NewMemoryStore(), rec)
	res, err := exec.Run(d)
	if err != nil {
		t.Fatal(err)
	}
	for id, st := range res.Statuses {
		if st != StatusSuccess {
			t.Fatalf("node %s status=%s", id, st)
		}
	}
	for id, n := range writes {
		if n != 1 {
			t.Fatalf("node %s wrote %d times, want exactly 1", id, n)
		}
	}
}

func TestRetryAndIdempotentReuse(t *testing.T) {
	var attempts int
	var mu sync.Mutex
	n := Node{
		ID: "flaky", InputSummary: "in", ParamSummary: "p=1",
		Run: func(ctx *RunContext) error {
			mu.Lock()
			attempts++
			a := attempts
			mu.Unlock()
			if a < 3 {
				return errors.New("transient")
			}
			return ctx.SaveArtifact([]byte("stable-output"))
		},
	}
	d, _ := NewDAG([]Node{n})
	rec := core.NewRecorder()
	store := NewMemoryStore()
	exec := NewExecutor(Config{Workers: 1, MaxRetry: 2}, store, rec)
	res, err := exec.Run(d)
	if err != nil {
		t.Fatal(err)
	}
	if res.Statuses["flaky"] != StatusSuccess {
		t.Fatalf("status=%s", res.Statuses["flaky"])
	}
	key := n.Key()
	hash1 := store.Hash(key)
	if hash1 == "" {
		t.Fatal("artifact not persisted")
	}
	// Second run must reuse the artifact and never re-execute.
	exec2 := NewExecutor(Config{Workers: 1, MaxRetry: 2}, store, core.NewRecorder())
	res2, _ := exec2.Run(d)
	if res2.RetriedReused != 1 {
		t.Fatalf("reused=%d want 1", res2.RetriedReused)
	}
	if store.Hash(key) != hash1 {
		t.Fatal("resume changed the artifact hash")
	}
}

func TestFirstBlockedOnFailure(t *testing.T) {
	boom := errors.New("boom")
	nodes := []Node{
		{ID: "recover", Run: func(*RunContext) error { return boom }},
		{ID: "inspect", Deps: []string{"recover"}, Run: noopRun},
		{ID: "metrics", Deps: []string{"inspect"}, Run: noopRun},
		{ID: "remux", Deps: []string{"metrics"}, Run: noopRun},
	}
	d, _ := NewDAG(nodes)
	rec := core.NewRecorder()
	exec := NewExecutor(Config{Workers: 2, MaxRetry: 0}, NewMemoryStore(), rec)
	res, err := exec.Run(d)
	if err != nil {
		t.Fatal(err)
	}
	if res.Statuses["recover"] != StatusFailed {
		t.Fatalf("recover status=%s", res.Statuses["recover"])
	}
	if res.Statuses["inspect"] != StatusBlocked ||
		res.Statuses["metrics"] != StatusBlocked ||
		res.Statuses["remux"] != StatusBlocked {
		t.Fatalf("downstream must be blocked: %+v", res.Statuses)
	}
	if res.FirstBlocked != "inspect" {
		t.Fatalf("first blocked=%q want inspect", res.FirstBlocked)
	}
	if len(res.Failed) != 1 || res.Failed[0] != "recover" {
		t.Fatalf("failed list wrong: %v", res.Failed)
	}
}

func TestRetryExhaustionLeavesFailed(t *testing.T) {
	n := Node{ID: "always-bad", Run: func(*RunContext) error { return errors.New("nope") }}
	d, _ := NewDAG([]Node{n})
	exec := NewExecutor(Config{Workers: 1, MaxRetry: 3}, NewMemoryStore(), core.NewRecorder())
	res, _ := exec.Run(d)
	if res.Statuses["always-bad"] != StatusFailed {
		t.Fatalf("status=%s want failed", res.Statuses["always-bad"])
	}
}

func TestFileStoreResumeAcrossInstances(t *testing.T) {
	dir := t.TempDir()
	n := Node{ID: "persist", InputSummary: "x", Run: func(ctx *RunContext) error {
		return ctx.SaveArtifact([]byte("durable"))
	}}
	d, _ := NewDAG([]Node{n})
	s1, _ := NewFileStore(dir)
	exec1 := NewExecutor(Config{Workers: 1, MaxRetry: 0}, s1, core.NewRecorder())
	if _, err := exec1.Run(d); err != nil {
		t.Fatal(err)
	}
	// A brand-new executor/store over the same directory resumes from disk.
	s2, _ := NewFileStore(dir)
	exec2 := NewExecutor(Config{Workers: 1, MaxRetry: 0}, s2, core.NewRecorder())
	res, _ := exec2.Run(d)
	if res.RetriedReused != 1 {
		t.Fatalf("disk resume reused=%d", res.RetriedReused)
	}
	if s2.Hash(n.Key()) != s1.Hash(n.Key()) {
		t.Fatal("cross-instance artifact hash mismatch")
	}
}

func TestIdempotencyKeyStability(t *testing.T) {
	n := Node{ID: "n", InputSummary: "abc", ParamSummary: "w=4"}
	k1, k2 := n.Key(), n.Key()
	if k1 != k2 || len(k1) != 16 {
		t.Fatalf("key unstable or wrong shape: %q %q", k1, k2)
	}
	mutated := n
	mutated.ParamSummary = "w=8"
	if mutated.Key() == k1 {
		t.Fatal("changed parameters must change the idempotency key")
	}
}

func noopRun(*RunContext) error { return nil }

func makeNodesWithWrites(n int, mu *sync.Mutex, writes map[string]int) []Node {
	var nodes []Node
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("node-%d", i)
		var deps []string
		if i > 0 {
			deps = []string{fmt.Sprintf("node-%d", i-1)}
		}
		nodes = append(nodes, Node{
			ID: id, Deps: deps,
			Run: func(ctx *RunContext) error {
				mu.Lock()
				writes[id]++
				mu.Unlock()
				return ctx.SaveArtifact([]byte(id))
			},
		})
	}
	return nodes
}
