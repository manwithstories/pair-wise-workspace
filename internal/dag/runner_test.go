package dag

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"review/internal/media"
)

func TestIdempotencyKeyStable(t *testing.T) {
	inputs := map[string]string{"a": "hash-a", "b": "hash-b"}
	params := struct {
		Rate int `json:"rate"`
	}{Rate: 48000}
	k1, err := IdempotencyKey(inputs, params)
	if err != nil {
		t.Fatal(err)
	}
	// Different map iteration order must not change the key.
	again, _ := IdempotencyKey(map[string]string{"b": "hash-b", "a": "hash-a"}, params)
	if k1 != again {
		t.Fatal("idempotency key sensitive to input map order")
	}
	changed, _ := IdempotencyKey(inputs, struct{ Rate int }{Rate: 44100})
	if changed == k1 {
		t.Fatal("different parameters produced the same key")
	}
}

func TestRetrySucceedsAndDoesNotDoubleWrite(t *testing.T) {
	dir := t.TempDir()
	rt, err := NewRunner(dir)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	writes := 0
	var attempts int
	node := Node{
		Name: "flaky", MaxAttempts: 3,
		Run: func(rt *Runner) error {
			mu.Lock()
			attempts++
			a := attempts
			mu.Unlock()
			if a < 3 {
				return media.NewInternal("flaky", "transient", errors.New("io"))
			}
			// A successful commit writes exactly once.
			writes++
			rt.RecordOutput("flaky", "artifact", "hash-1")
			return nil
		},
	}
	if err := rt.Add(node); err != nil {
		t.Fatal(err)
	}
	if err := rt.RunAll(); err != nil {
		t.Fatalf("retry should converge: %v", err)
	}
	if attempts != 3 || writes != 1 {
		t.Fatalf("attempts=%d writes=%d, want 3 and 1", attempts, writes)
	}
	st, ok := rt.State("flaky")
	if !ok || st.Status != StatusSucceeded || st.Attempts != 3 {
		t.Fatalf("state = %+v", st)
	}
}

func TestPersistentFailureMarksFailed(t *testing.T) {
	rt, _ := NewRunner(t.TempDir())
	_ = rt.Add(Node{Name: "bad", MaxAttempts: 2, Run: func(*Runner) error {
		return media.NewInternal("bad", "always fails", errors.New("disk"))
	}})
	err := rt.RunAll()
	var pe *media.PipelineError
	if !errors.As(err, &pe) || pe.Class != media.ClassInternal {
		t.Fatalf("want internal pipeline error, got %v", err)
	}
	st, _ := rt.State("bad")
	if st.Status != StatusFailed || st.Attempts != 2 {
		t.Fatalf("state = %+v", st)
	}
}

func TestResumeSkipsSucceededNodes(t *testing.T) {
	dir := t.TempDir()
	build := func() *Runner {
		rt, err := NewRunner(dir)
		if err != nil {
			t.Fatal(err)
		}
		return rt
	}
	// First run: complete node A, leave B unexecuted (simulated crash by
	// simply not adding B to the first runner).
	rt1 := build()
	runsA := 0
	_ = rt1.Add(Node{Name: "A", Run: func(rt *Runner) error {
		runsA++
		key, _ := IdempotencyKey(map[string]string{"in": "x"}, nil)
		rt.MarkKey("A", key)
		rt.RecordOutput("A", "out", "digest")
		return nil
	}})
	if err := rt1.RunAll(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "dag-state.json")); err != nil {
		t.Fatalf("state not persisted: %v", err)
	}
	// Second run from the same state directory: A must be skipped.
	rt2 := build()
	_ = rt2.Add(Node{Name: "A", Run: func(*Runner) error {
		t.Fatal("succeeded node re-ran on resume")
		return nil
	}})
	runsB := 0
	_ = rt2.Add(Node{Name: "B", Deps: []string{"A"}, Run: func(rt *Runner) error {
		runsB++
		if v, ok := rt.Output("A", "out"); !ok || v != "digest" {
			t.Fatal("upstream output not recovered from durable state")
		}
		return nil
	}})
	if err := rt2.RunAll(); err != nil {
		t.Fatalf("resume run failed: %v", err)
	}
	if runsA != 1 || runsB != 1 {
		t.Fatalf("runs A=%d B=%d, want 1 and 1", runsA, runsB)
	}
}

func TestBlockedDependencyPropagates(t *testing.T) {
	rt, _ := NewRunner(t.TempDir())
	rootErr := media.NewConflict("up", "unmappable audio", "mkv-decode")
	_ = rt.Add(Node{Name: "up", MaxAttempts: 1, Run: func(*Runner) error {
		return rootErr
	}})
	downRan := false
	_ = rt.Add(Node{Name: "down", Deps: []string{"up"}, Run: func(*Runner) error {
		downRan = true
		return nil
	}})
	err := rt.RunAll()
	var pe *media.PipelineError
	if !errors.As(err, &pe) {
		t.Fatalf("expected pipeline error, got %v", err)
	}
	// Fail-fast: the upstream failure aborts the run and the dependent
	// never executes, staying pending rather than producing partial output.
	if downRan {
		t.Fatal("downstream ran despite failed dependency")
	}
	st, _ := rt.State("down")
	if st.Status != StatusPending {
		t.Fatalf("downstream status = %s, want pending under fail-fast", st.Status)
	}
	// The root conflict itself retains the first blocker provenance.
	if media.FirstBlocker(rootErr) != "mkv-decode" {
		t.Fatalf("root blocker = %q, want mkv-decode", media.FirstBlocker(rootErr))
	}
	// And the explicit BlockedBy wrapper preserves the full chain.
	wrapped := media.BlockedBy("down", rootErr)
	if media.FirstBlocker(wrapped) != "mkv-decode" {
		t.Fatalf("wrapped blocker = %q, want mkv-decode", media.FirstBlocker(wrapped))
	}
}

func TestBlockedStatusWhenDependencyMissing(t *testing.T) {
	// When the dependency node exists in the graph but cannot succeed in a
	// later resume (its state is failed), the dependent is marked blocked
	// with a wrapped chain.
	dir := t.TempDir()
	rt1, _ := NewRunner(dir)
	rootErr := media.NewConflict("up", "unmappable", "mkv-decode")
	_ = rt1.Add(Node{Name: "up", MaxAttempts: 1, Run: func(*Runner) error {
		return rootErr
	}})
	_ = rt1.RunAll()

	rt2, _ := NewRunner(dir)
	_ = rt2.Add(Node{Name: "up", MaxAttempts: 1, Run: func(*Runner) error {
		return rootErr
	}})
	_ = rt2.Add(Node{Name: "down", Deps: []string{"up"}, Run: func(*Runner) error {
		t.Fatal("must not run")
		return nil
	}})
	// rt2 reloads "up" as failed from durable state, so down is blocked.
	err := rt2.RunAll()
	if err == nil {
		t.Fatal("expected blocking error on resume")
	}
	st, _ := rt2.State("down")
	if st.Status != StatusBlocked {
		t.Fatalf("status = %s, want blocked", st.Status)
	}
	if media.FirstBlocker(err) != "mkv-decode" {
		t.Fatalf("blocker = %q, want mkv-decode", media.FirstBlocker(err))
	}
}

func TestSucceededWithKeyGuardsRecompute(t *testing.T) {
	rt, _ := NewRunner(t.TempDir())
	key, _ := IdempotencyKey(map[string]string{"x": "1"}, nil)
	_ = rt.Add(Node{Name: "n", Run: func(rt *Runner) error {
		rt.MarkKey("n", key)
		return nil
	}})
	if err := rt.RunAll(); err != nil {
		t.Fatal(err)
	}
	if !rt.IsSucceededWith("n", key) {
		t.Fatal("matching key must report succeeded")
	}
	if rt.IsSucceededWith("n", "other-key") {
		t.Fatal("different key must not be treated as succeeded")
	}
}
