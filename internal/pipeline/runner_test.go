package pipeline

import (
	"os"
	"path/filepath"
	"testing"

	"mediacore/internal/provenance"
	"mediacore/internal/store"
)

type numPayload struct {
	Value int `json:"value"`
}

func testNodes(counter *map[string]int) []Node {
	return []Node{
		{
			Name: "a", Version: "1",
			New: func() any { return &numPayload{} },
			Run: func(ctx *Context) (any, error) {
				(*counter)["a"]++
				return &numPayload{Value: 1}, nil
			},
		},
		{
			Name: "b", Deps: []string{"a"}, Version: "1",
			New: func() any { return &numPayload{} },
			Run: func(ctx *Context) (any, error) {
				(*counter)["b"]++
				return &numPayload{Value: ctx.Get("a").(*numPayload).Value + 1}, nil
			},
		},
		{
			Name: "c", Deps: []string{"b"}, Version: "1",
			New: func() any { return &numPayload{} },
			Run: func(ctx *Context) (any, error) {
				(*counter)["c"]++
				return &numPayload{Value: ctx.Get("b").(*numPayload).Value + 1}, nil
			},
		},
	}
}

func newCtx(t *testing.T, dir string) *Context {
	t.Helper()
	st, err := store.Open(filepath.Join(dir, "checkpoints"))
	if err != nil {
		t.Fatal(err)
	}
	return &Context{
		Store:        st,
		Prov:         provenance.Open(filepath.Join(dir, "prov.jsonl")),
		InputDigest:  "input",
		ParamsDigest: "params",
	}
}

func TestRunCommitsThenReuses(t *testing.T) {
	dir := t.TempDir()
	counter := map[string]int{}
	r, err := New(testNodes(&counter))
	if err != nil {
		t.Fatal(err)
	}
	sum, err := r.Run(newCtx(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	if sum.Commits != 3 || sum.Reused != 0 {
		t.Fatalf("first run: %+v", sum)
	}
	sum, err = r.Run(newCtx(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	if sum.Commits != 0 || sum.Reused != 3 || sum.DuplicateCommits != 0 {
		t.Fatalf("second run should reuse everything: %+v", sum)
	}
	if counter["a"] != 1 || counter["c"] != 1 {
		t.Fatalf("nodes must run once: %v", counter)
	}
}

func TestParamChangeRecomputes(t *testing.T) {
	dir := t.TempDir()
	counter := map[string]int{}
	r, _ := New(testNodes(&counter))
	r.Run(newCtx(t, dir))
	ctx := newCtx(t, dir)
	ctx.ParamsDigest = "other"
	sum, err := r.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Commits != 3 {
		t.Fatalf("param change must recompute all: %+v", sum)
	}
}

func TestCorruptCheckpointRollback(t *testing.T) {
	dir := t.TempDir()
	counter := map[string]int{}
	r, _ := New(testNodes(&counter))
	if _, err := r.Run(newCtx(t, dir)); err != nil {
		t.Fatal(err)
	}
	// Corrupt node b's checkpoint.
	p := filepath.Join(dir, "checkpoints", "b.json")
	data, _ := os.ReadFile(p)
	data[len(data)-3] ^= 0xFF
	os.WriteFile(p, data, 0o644)

	ctx := newCtx(t, dir)
	sum, err := r.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// a is reused; b and c are rolled back and re-executed.
	if sum.RecoveredNodes != 2 {
		t.Fatalf("recovered = %d, want 2", sum.RecoveredNodes)
	}
	if sum.FirstMissingDependency != "b" || sum.RecoveryPosition != "b" {
		t.Fatalf("recovery should point at b: %+v", sum)
	}
	if counter["a"] != 1 || counter["b"] != 2 || counter["c"] != 2 {
		t.Fatalf("counters: %v", counter)
	}
	if len(ctx.ErrorChain) == 0 {
		t.Fatal("corrupt event should be recorded in the error chain")
	}
	// Final state must be consistent again.
	if got := ctx.Get("c").(*numPayload).Value; got != 3 {
		t.Fatalf("c = %d, want 3", got)
	}
}

func TestMissingDependencyFails(t *testing.T) {
	r, err := New([]Node{{
		Name: "x", Deps: []string{"ghost"}, Version: "1",
		New: func() any { return &numPayload{} },
		Run: func(ctx *Context) (any, error) { return &numPayload{}, nil },
	}})
	if err == nil {
		_ = r
		t.Fatal("unknown dependency must fail at graph build")
	}
}
