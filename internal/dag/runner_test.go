package dag

import (
	"errors"
	"sync"
	"testing"

	"mediad/internal/model"
)

type memStore struct {
	mu    sync.Mutex
	data  map[string]string
	saves int
}

func newMemStore() *memStore { return &memStore{data: map[string]string{}} }

func (s *memStore) Load(id string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data[id], nil
}

func (s *memStore) Save(id, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data[id] == key {
		return errors.New("duplicate output: same idempotent key saved twice")
	}
	if _, exists := s.data[id]; !exists {
		s.saves++
	}
	s.data[id] = key // changed key overwrites the invalidated record
	return nil
}

func diamond() []Node {
	return []Node{
		{ID: "a", InputDigest: "da", Run: noop()},
		{ID: "b", Inputs: []string{"a"}, InputDigest: "db", Run: noop()},
		{ID: "c", Inputs: []string{"a"}, InputDigest: "dc", Run: noop()},
		{ID: "d", Inputs: []string{"b", "c"}, InputDigest: "dd", Run: noop()},
	}
}

func TestRun_AllSuccessAndOrder(t *testing.T) {
	g, _ := NewGraph(diamond())
	store := newMemStore()
	results, err := NewRunner(g, store).Run(Options{Concurrency: 4, MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range results {
		if r.Status != StatusSuccess {
			t.Fatalf("node %s status %s: %s", r.ID, r.Status, r.Error)
		}
	}
	if store.saves != 4 {
		t.Fatalf("want exactly 4 checkpoint saves, got %d", store.saves)
	}
}

func TestRun_CheckpointSkipsAndNoDuplicateOutput(t *testing.T) {
	g, _ := NewGraph(diamond())
	store := newMemStore()
	runner := NewRunner(g, store)
	if _, err := runner.Run(Options{Concurrency: 2, MaxAttempts: 1}); err != nil {
		t.Fatal(err)
	}
	// Second run: every key matches, nothing may be produced again.
	results, err := runner.Run(Options{Concurrency: 2, MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range results {
		if r.Status != StatusSkipped {
			t.Fatalf("rerun node %s = %s, want skipped", r.ID, r.Status)
		}
	}
	if store.saves != 4 {
		t.Fatalf("retry must not duplicate outputs, saves=%d", store.saves)
	}
}

func TestRun_ChangedDigestInvalidates(t *testing.T) {
	g1, _ := NewGraph(diamond())
	store := newMemStore()
	if _, err := NewRunner(g1, store).Run(Options{MaxAttempts: 1}); err != nil {
		t.Fatal(err)
	}
	// Rebuild with changed leaf digest; its key changes, upstream keys do not.
	nodes := diamond()
	nodes[3].InputDigest = "dd-CHANGED"
	g2, _ := NewGraph(nodes)
	results, err := NewRunner(g2, store).Run(Options{MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Status{}
	for _, r := range results {
		got[r.ID] = r.Status
	}
	for _, id := range []string{"a", "b", "c"} {
		if got[id] != StatusSkipped {
			t.Fatalf("unchanged node %s must stay checkpointed, got %s", id, got[id])
		}
	}
	if got["d"] != StatusSuccess {
		t.Fatalf("changed leaf must re-run, got %s", got["d"])
	}
}

func TestRun_FailureBlocksDependentsAndNamesFirstBlocker(t *testing.T) {
	boom := model.NewMediaError("test", model.TrackStudio, 0, 42, true, model.ErrBadSegment)
	nodes := diamond()
	nodes[1].Run = func(*RunContext) error { return boom } // b fails
	g, _ := NewGraph(nodes)
	results, err := NewRunner(g, newMemStore()).Run(Options{Concurrency: 4, MaxAttempts: 2})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Result{}
	for _, r := range results {
		got[r.ID] = r
	}
	if got["b"].Status != StatusFailed || got["b"].Attempts != 2 || !got["b"].Recoverable {
		t.Fatalf("b must fail after retries: %+v", got["b"])
	}
	if got["d"].Status != StatusBlocked {
		t.Fatalf("d must be blocked, got %+v", got["d"])
	}
	if got["a"].Status != StatusSuccess || got["c"].Status != StatusSuccess {
		t.Fatalf("independent branch must finish: %+v", got)
	}
	id, st := FirstBlocked(results)
	if id != "b" || st != StatusFailed {
		t.Fatalf("first blocker = %s/%s, want b/failed", id, st)
	}
}

func TestRun_RetrySucceedsWithinAttemptBudget(t *testing.T) {
	var mu sync.Mutex
	tries := 0
	nodes := diamond()
	nodes[0].Run = func(*RunContext) error {
		mu.Lock()
		defer mu.Unlock()
		tries++
		if tries < 3 {
			return errors.New("transient")
		}
		return nil
	}
	g, _ := NewGraph(nodes)
	results, err := NewRunner(g, newMemStore()).Run(Options{MaxAttempts: 3})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Status != StatusSuccess || results[0].Attempts != 3 {
		t.Fatalf("flaky node should succeed on attempt 3: %+v", results[0])
	}
}

func TestRun_InterruptThenResume(t *testing.T) {
	g, _ := NewGraph(diamond())
	store := newMemStore()
	// Stop scheduling once two nodes have completed.
	first, err := NewRunner(g, store).Run(Options{
		Concurrency: 1, MaxAttempts: 1,
		Interrupt: func(done int) bool { return done >= 2 },
	})
	if err != nil {
		t.Fatal(err)
	}
	id, st := FirstBlocked(first)
	if id == "" || st != StatusBlocked {
		t.Fatalf("interrupted run must leave a blocked node, got %s/%s", id, st)
	}
	// Resume without interruption: checkpointed nodes skip, the rest finish.
	second, err := NewRunner(g, store).Run(Options{Concurrency: 1, MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range second {
		if r.Status != StatusSuccess && r.Status != StatusSkipped {
			t.Fatalf("resume left node %s in %s: %s", r.ID, r.Status, r.Error)
		}
	}
	if id2, _ := FirstBlocked(second); id2 != "" {
		t.Fatalf("resumed run must be unblocked, blocker=%s", id2)
	}
}
