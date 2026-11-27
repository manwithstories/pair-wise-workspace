package recovery

import (
	"os"
	"path/filepath"
	"testing"

	"fastpost/internal/metadata"
)

func state(id, input, cfg string) NodeState {
	return NodeState{
		NodeID: id, InputDigest: input, ConfigDigest: cfg,
		ParserVer: metadata.ParserVersion, OutputDigest: "out-" + id, Done: true,
	}
}

// downstream mirrors a diamond: src -> norm -> align -> manifest.
func downstream(id string) []string {
	chain := map[string][]string{
		"src":      {"src", "norm", "align", "manifest"},
		"norm":     {"norm", "align", "manifest"},
		"align":    {"align", "manifest"},
		"manifest": {"manifest"},
	}
	return chain[id]
}

var topo = []string{"src", "norm", "align", "manifest"}

func TestFreshRunRedoesEverything(t *testing.T) {
	desired := map[string]NodeState{
		"src":      state("src", "i1", "c1"),
		"norm":     state("norm", "i2", "c1"),
		"align":    state("align", "i3", "c1"),
		"manifest": state("manifest", "i4", "c1"),
	}
	plan := BuildPlan(nil, desired, topo, downstream)
	if len(plan.Reusable) != 0 || len(plan.Redo) != 4 {
		t.Fatalf("fresh run: reusable=%v redo=%v", plan.Reusable, plan.Redo)
	}
}

func TestUnchangedRunReusesAll(t *testing.T) {
	cp := &Checkpoint{RunID: "r", Nodes: []NodeState{
		state("src", "i1", "c1"),
		state("norm", "i2", "c1"),
		state("align", "i3", "c1"),
		state("manifest", "i4", "c1"),
	}}
	desired := map[string]NodeState{}
	for _, n := range cp.Nodes {
		desired[n.NodeID] = n
	}
	plan := BuildPlan(cp, desired, topo, downstream)
	if len(plan.Redo) != 0 || len(plan.Reusable) != 4 {
		t.Fatalf("unchanged run: reusable=%v redo=%v", plan.Reusable, plan.Redo)
	}
}

func TestInputChangeRedoesOnlyDownstream(t *testing.T) {
	cp := &Checkpoint{Nodes: []NodeState{
		state("src", "old-input", "c1"),
		state("norm", "i2", "c1"),
		state("align", "i3", "c1"),
		state("manifest", "i4", "c1"),
	}}
	desired := map[string]NodeState{
		"src":      state("src", "new-input", "c1"), // changed
		"norm":     state("norm", "i2", "c1"),
		"align":    state("align", "i3", "c1"),
		"manifest": state("manifest", "i4", "c1"),
	}
	plan := BuildPlan(cp, desired, topo, downstream)
	want := map[string]bool{"src": true, "norm": true, "align": true, "manifest": true}
	if len(plan.Redo) != 4 {
		t.Fatalf("redo = %v, want full downstream closure", plan.Redo)
	}
	for _, id := range plan.Redo {
		if !want[id] {
			t.Fatalf("unexpected redo node %s", id)
		}
	}
}

func TestParserVersionInvalidates(t *testing.T) {
	old := state("src", "i1", "c1")
	old.ParserVer = "fp-parser-0" // older parser
	cp := &Checkpoint{Nodes: []NodeState{old}}
	desired := map[string]NodeState{"src": state("src", "i1", "c1")}
	plan := BuildPlan(cp, desired, topo, downstream)
	// src is invalidated, and its whole downstream closure must redo too.
	if len(plan.Redo) != 4 {
		t.Fatalf("old parser version must invalidate src and downstream, got redo=%v", plan.Redo)
	}
}

func TestConfigChangeInvalidates(t *testing.T) {
	cp := &Checkpoint{Nodes: []NodeState{state("align", "i3", "old-cfg")}}
	desired := map[string]NodeState{"align": state("align", "i3", "new-cfg")}
	plan := BuildPlan(cp, desired, topo, downstream)
	// align plus its downstream manifest must redo.
	if len(plan.Redo) != 2 {
		t.Fatalf("redo = %v, want align+manifest", plan.Redo)
	}
}

func TestStoreSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	missing, err := store.Load()
	if err != nil || missing != nil {
		t.Fatalf("missing checkpoint should load nil, got %v %v", missing, err)
	}
	cp := &Checkpoint{RunID: "run-1", Nodes: []NodeState{state("src", "i", "c")}}
	if err := store.Save(cp); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.RunID != "run-1" || len(loaded.Nodes) != 1 || loaded.Nodes[0].NodeID != "src" {
		t.Fatalf("round trip mismatch: %+v", loaded)
	}
	// No leftover temp file.
	matches, _ := filepath.Glob(filepath.Join(dir, "*.tmp"))
	if len(matches) != 0 {
		t.Fatalf("temp files remain: %v", matches)
	}
}

func TestStoreCorruptCheckpoint(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	if err := store.Save(&Checkpoint{RunID: "x"}); err != nil {
		t.Fatal(err)
	}
	// Corrupt the file after a valid save.
	path := filepath.Join(dir, checkpointFile)
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(); err == nil {
		t.Fatal("corrupt checkpoint must return an error")
	}
}

func TestDigestStableAndDistinct(t *testing.T) {
	a := Digest([]byte("input"), []byte("cfg"))
	b := Digest([]byte("input"), []byte("cfg"))
	if a != b || len(a) != 64 {
		t.Fatal("identical parts must give a stable 64-char digest")
	}
	c := Digest([]byte("input"), []byte("other"))
	if a == c {
		t.Fatal("different parts must give different digests")
	}
}
