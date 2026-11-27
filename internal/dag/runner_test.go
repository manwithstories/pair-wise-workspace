package dag

import (
	"errors"
	"testing"
)

type fakeNode struct {
	name    string
	deps    []string
	digests map[string]string
	params  map[string]string
	fail    bool
	runs    int
}

func (f *fakeNode) Name() string                          { return f.name }
func (f *fakeNode) DependsOn() []string                   { return f.deps }
func (f *fakeNode) InputDigests() map[string]string       { return f.digests }
func (f *fakeNode) Parameters() map[string]string         { return f.params }
func (f *fakeNode) Run(*Context) (string, error) {
	f.runs++
	if f.fail {
		return "", errors.New("boom")
	}
	return "out:" + f.name, nil
}

func TestRunnerTopologyAndIdempotentResume(t *testing.T) {
	dir := t.TempDir()
	j, _ := OpenJournal(dir)

	a := &fakeNode{name: "ingest", digests: map[string]string{"in": "x"}}
	b := &fakeNode{name: "recover", deps: []string{"ingest"}}
	c := &fakeNode{name: "pack", deps: []string{"recover"}}
	for _, n := range []*fakeNode{a, b, c} {
		n.digests = map[string]string{"in": "x"}
	}

	r := NewRunner(j, "")
	res, err := r.Run([]Node{a, b, c})
	if err != nil || len(res.Executed) != 3 {
		t.Fatalf("first run: %+v err=%v", res, err)
	}

	// A fresh runner over the same journal reuses every succeeded node.
	r2 := NewRunner(j, "")
	res2, err := r2.Run([]Node{a, b, c})
	if err != nil {
		t.Fatal(err)
	}
	if len(res2.Skipped) != 3 || len(res2.Executed) != 0 {
		t.Fatalf("resume must skip all: %+v", res2)
	}
	for _, n := range []*fakeNode{a, b, c} {
		if n.runs != 1 {
			t.Fatalf("node %s ran %d times, want 1", n.name, n.runs)
		}
	}
}

func TestRunnerFailureBlocksDependents(t *testing.T) {
	dir := t.TempDir()
	j, _ := OpenJournal(dir)
	a := &fakeNode{name: "ingest", fail: true, digests: map[string]string{"in": "x"}}
	b := &fakeNode{name: "pack", deps: []string{"ingest"}, digests: map[string]string{"in": "x"}}
	r := NewRunner(j, "")
	res, err := r.Run([]Node{a, b})
	if err != nil {
		t.Fatalf("graph should collect failure, not crash: %v", err)
	}
	if res.FirstBlock == nil || res.FirstBlock.Node != "pack" || res.FirstBlock.By != "ingest" {
		t.Fatalf("first block = %+v", res.FirstBlock)
	}
	if b.runs != 0 {
		t.Fatal("blocked dependent must never execute")
	}
	// The failed node's journal entry records the failure.
	snap := j.Snapshot()
	var sawFailed bool
	for _, e := range snap {
		if e.Name == "ingest" && e.Status == StatusFailed && e.Error == "boom" {
			sawFailed = true
		}
	}
	if !sawFailed {
		t.Fatalf("failed node not journaled: %+v", snap)
	}
}

func TestRunnerPauseAfter(t *testing.T) {
	dir := t.TempDir()
	j, _ := OpenJournal(dir)
	a := &fakeNode{name: "ingest", digests: map[string]string{"in": "x"}}
	b := &fakeNode{name: "sync", deps: []string{"ingest"}, digests: map[string]string{"in": "x"}}
	c := &fakeNode{name: "pack", deps: []string{"sync"}, digests: map[string]string{"in": "x"}}
	r := NewRunner(j, "sync")
	res, err := r.Run([]Node{a, b, c})
	var pe *PauseError
	if !errors.As(err, &pe) || res.PausedAt != "sync" {
		t.Fatalf("want PauseError at sync, got %+v err=%v", res, err)
	}
	if c.runs != 0 {
		t.Fatal("node after pause must not run")
	}
	// Resume without a pause point completes the remaining node only.
	r2 := NewRunner(j, "")
	res2, err := r2.Run([]Node{a, b, c})
	if err != nil {
		t.Fatal(err)
	}
	if len(res2.Skipped) != 2 || len(res2.Executed) != 1 || res2.Executed[0] != "pack" {
		t.Fatalf("resume after pause: %+v", res2)
	}
}

func TestRunnerCycleDetected(t *testing.T) {
	dir := t.TempDir()
	j, _ := OpenJournal(dir)
	a := &fakeNode{name: "a", deps: []string{"b"}, digests: map[string]string{"in": "x"}}
	b := &fakeNode{name: "b", deps: []string{"a"}, digests: map[string]string{"in": "x"}}
	if _, err := NewRunner(j, "").Run([]Node{a, b}); err == nil {
		t.Fatal("dependency cycle must be rejected")
	}
}
