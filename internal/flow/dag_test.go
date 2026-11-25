package flow

import (
	"errors"
	"testing"

	"github.com/pairwise/mediad/internal/media"
	"github.com/pairwise/mediad/internal/store"
)

func TestTopologicalOrderStable(t *testing.T) {
	d := New()
	_ = d.Add(&Node{ID: "a"})
	_ = d.Add(&Node{ID: "b", Deps: []string{"a"}})
	_ = d.Add(&Node{ID: "c", Deps: []string{"a", "b"}})
	order, err := d.TopologicalOrder()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a", "b", "c"}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order=%v want %v", order, want)
		}
	}
}

func TestCycleRejected(t *testing.T) {
	d := New()
	_ = d.Add(&Node{ID: "a", Deps: []string{"b"}})
	_ = d.Add(&Node{ID: "b", Deps: []string{"a"}})
	if _, err := d.TopologicalOrder(); media.ExitCode(err) != 4 {
		t.Fatalf("cycle err=%v", err)
	}
}

func newTestExecutor(t *testing.T) *Executor {
	t.Helper()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e, err := NewExecutor(st, "dag-state.json")
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestExecutorIdempotentRetryAndResume(t *testing.T) {
	e := newTestExecutor(t)
	calls := 0
	d := New()
	if err := d.Add(&Node{
		ID: "seed", OutputRel: "seed.bin", Retries: 1,
		Inputs: map[string]string{"v": "1"},
		Produce: func() ([]byte, error) {
			calls++
			if calls == 1 {
				return nil, errors.New("temporary write hiccup")
			}
			return []byte("deterministic"), nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.Run(d); err != nil {
		t.Fatal(err)
	}
	n := d.Get("seed")
	if n.Attempts != 2 || n.SHA256 == "" {
		t.Fatalf("attempts=%d hash=%s", n.Attempts, n.SHA256)
	}
	firstHash := n.SHA256

	// A new executor simulates a process retry. Matching completed work is cached.
	e2, err := NewExecutor(e.Store, "dag-state.json")
	if err != nil {
		t.Fatal(err)
	}
	d2 := New()
	if err := d2.Add(&Node{
		ID: "seed", OutputRel: "seed.bin", Inputs: map[string]string{"v": "1"},
		Produce: func() ([]byte, error) { t.Fatal("completed node must not rerun"); return nil, nil },
	}); err != nil {
		t.Fatal(err)
	}
	if err := e2.Run(d2); err != nil {
		t.Fatal(err)
	}
	if e2.Cached != 1 || d2.Get("seed").SHA256 != firstHash {
		t.Fatalf("cached=%d hash stable=%v", e2.Cached, d2.Get("seed").SHA256 == firstHash)
	}
}

func TestBlockedRootIsStableFirstBlockedAndResumeRunsOnlyUnfinished(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e, _ := NewExecutor(st, "state.json")
	d := New()
	blockedProduce := func() ([]byte, error) {
		return nil, media.NewError(media.CodeBlockedDependency, "repair", "init", "missing init")
	}
	_ = d.Add(&Node{ID: "repair:track-02:init-segment", OutputRel: "init.mp4", Produce: blockedProduce})
	_ = d.Add(&Node{ID: "repair:track-02:segment-01", Deps: []string{"repair:track-02:init-segment"}, Produce: func() ([]byte, error) { return []byte("x"), nil }})
	err = e.Run(d)
	if media.ExitCode(err) != 3 {
		t.Fatalf("err=%v exit=%d", err, media.ExitCode(err))
	}
	first, err := d.FirstBlocked()
	if err != nil || first != "repair:track-02:init-segment" {
		t.Fatalf("first=%q err=%v", first, err)
	}
	if d.Get("repair:track-02:segment-01").State != StateBlocked {
		t.Fatal("downstream must be blocked")
	}

	// Supply the missing initialization and resume with a fresh executor.
	e2, _ := NewExecutor(st, "state.json")
	d2 := New()
	_ = d2.Add(&Node{ID: "repair:track-02:init-segment", OutputRel: "init.mp4", Produce: func() ([]byte, error) { return []byte("init"), nil }})
	childRuns := 0
	_ = d2.Add(&Node{ID: "repair:track-02:segment-01", Deps: []string{"repair:track-02:init-segment"}, Produce: func() ([]byte, error) { childRuns++; return []byte("seg"), nil }})
	if err := e2.Run(d2); err != nil {
		t.Fatal(err)
	}
	if childRuns != 1 || d2.Get("repair:track-02:segment-01").State != StateDone {
		t.Fatalf("childRuns=%d state=%s", childRuns, d2.Get("repair:track-02:segment-01").State)
	}
}
