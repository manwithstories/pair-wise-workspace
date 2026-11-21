package dag

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"mediapack/internal/fault"
)

func stateAt(t *testing.T) *State {
	t.Helper()
	st, err := Load(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return st
}

func node(name string, deps []string, out string, runs *int) Node {
	return Node{
		Name: name, Deps: deps, Inputs: []string{"in-" + name}, Config: "cfg",
		Run: func() (string, error) {
			*runs++
			return out, nil
		},
	}
}

func TestRunExecutesInDependencyOrder(t *testing.T) {
	var order []string
	mk := func(name string, deps ...string) Node {
		return Node{Name: name, Deps: deps, Inputs: []string{"i"}, Config: "c",
			Run: func() (string, error) { order = append(order, name); return "h-" + name, nil }}
	}
	st := stateAt(t)
	res, err := Run([]Node{mk("manifest", "segments", "index"), mk("segments", "index"), mk("index")}, st, false)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(res.Executed) != 3 || order[0] != "index" || order[2] != "manifest" {
		t.Fatalf("order=%v executed=%v", order, res.Executed)
	}
	if res.Hashes["manifest"] != "h-manifest" {
		t.Fatalf("hashes=%v", res.Hashes)
	}
}

func TestRerunSkipsCompletedNodesAndPreservesHashes(t *testing.T) {
	var runs int
	st := stateAt(t)
	graph := func() []Node {
		return []Node{node("a", nil, "out-a", &runs), node("b", []string{"a"}, "out-b", &runs)}
	}
	first, err := Run(graph(), st, false)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Run(graph(), st, true)
	if err != nil {
		t.Fatal(err)
	}
	if runs != 2 {
		t.Fatalf("runs=%d want 2 (nodes must not run twice)", runs)
	}
	if len(second.Executed) != 0 || len(second.Skipped) != 2 || len(second.Resumed) != 2 {
		t.Fatalf("second run: %+v", second)
	}
	for name, hash := range first.Hashes {
		if second.Hashes[name] != hash {
			t.Fatalf("hash for %s changed across runs", name)
		}
	}
	if !AllDone(st, []string{"a", "b"}) {
		t.Fatal("state not fully done")
	}
}

func TestInterruptedRunResumesOnlyUnfinishedNodes(t *testing.T) {
	var runs int
	st := stateAt(t)
	crash := errors.New("simulated interruption")
	first := []Node{
		node("a", nil, "out-a", &runs),
		{Name: "b", Deps: []string{"a"}, Inputs: []string{"i"}, Config: "c",
			Run: func() (string, error) { runs++; return "", crash }},
		node("c", []string{"b"}, "out-c", &runs),
	}
	res, err := Run(first, st, false)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(res.Blocked) != 2 || res.FirstBlock == nil {
		t.Fatalf("blocked=%v first=%v", res.Blocked, res.FirstBlock)
	}
	if res.FirstBlock.BlockedBy != "dag:b" {
		t.Fatalf("first_blocked=%q", res.FirstBlock.BlockedBy)
	}
	if st.Nodes["b"].Status != StatusFailed || st.Nodes["c"].Status != StatusBlocked {
		t.Fatalf("state after crash: b=%s c=%s", st.Nodes["b"].Status, st.Nodes["c"].Status)
	}

	// Reload from disk: the interruption state must be durable.
	reloaded, err := Load(st.Path)
	if err != nil {
		t.Fatal(err)
	}
	resumedRuns := 0
	second := []Node{
		node("a", nil, "out-a", &resumedRuns),
		{Name: "b", Deps: []string{"a"}, Inputs: []string{"i"}, Config: "c",
			Run: func() (string, error) { resumedRuns++; return "out-b", nil }},
		node("c", []string{"b"}, "out-c", &resumedRuns),
	}
	res2, err := Run(second, reloaded, true)
	if err != nil {
		t.Fatal(err)
	}
	if resumedRuns != 2 {
		t.Fatalf("resumed runs=%d want 2 (b and c only)", resumedRuns)
	}
	if len(res2.Resumed) != 1 || res2.Resumed[0] != "a" {
		t.Fatalf("resumed=%v want [a]", res2.Resumed)
	}
	if len(res2.Executed) != 2 {
		t.Fatalf("executed=%v", res2.Executed)
	}
}

func TestKeyChangesWithInputsAndConfig(t *testing.T) {
	base := Node{Name: "a", Inputs: []string{"h1", "h2"}, Config: "c1"}
	if base.Key() != (Node{Name: "a", Inputs: []string{"h1", "h2"}, Config: "c1"}).Key() {
		t.Fatal("identical nodes have different keys")
	}
	if base.Key() == (Node{Name: "a", Inputs: []string{"h2", "h1"}, Config: "c1"}).Key() {
		t.Fatal("key ignores input order")
	}
	if base.Key() == (Node{Name: "a", Inputs: []string{"h1", "h2"}, Config: "c2"}).Key() {
		t.Fatal("key ignores config")
	}
	if base.Key() == (Node{Name: "a", Inputs: []string{"h1"}, Config: "c1"}).Key() {
		t.Fatal("length-prefixing failed: h1,h2 and h1 collided")
	}
}

func TestTopoRejectsCyclesAndMissingDeps(t *testing.T) {
	st := stateAt(t)
	var runs int
	cyclic := []Node{node("a", []string{"b"}, "x", &runs), node("b", []string{"a"}, "y", &runs)}
	if _, err := Run(cyclic, st, false); fault.ClassOf(err) != fault.ClassInput {
		t.Fatalf("cycle: %v", err)
	}
	missing := []Node{node("a", []string{"ghost"}, "x", &runs)}
	if _, err := Run(missing, st, false); fault.ClassOf(err) != fault.ClassInput {
		t.Fatalf("missing dep: %v", err)
	}
}

func TestDuplicateNodeRejected(t *testing.T) {
	var runs int
	if _, err := Run([]Node{node("a", nil, "x", &runs), node("a", nil, "y", &runs)}, stateAt(t), false); fault.ClassOf(err) != fault.ClassInput {
		t.Fatalf("got %v", err)
	}
}

func TestCorruptStateIsInputError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); fault.ClassOf(err) != fault.ClassInput {
		t.Fatalf("got %v", err)
	}
}

func TestSaveIsAtomicAndReloadable(t *testing.T) {
	st := stateAt(t)
	var runs int
	if _, err := Run([]Node{node("a", nil, "out-a", &runs)}, st, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(st.Path + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("temporary state file was left behind")
	}
	back, err := Load(st.Path)
	if err != nil {
		t.Fatal(err)
	}
	if back.Nodes["a"].Output != "out-a" || back.Nodes["a"].Status != StatusDone {
		t.Fatalf("state not persisted: %+v", back.Nodes["a"])
	}
}
