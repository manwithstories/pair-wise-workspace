package fixtures

import (
	"testing"
)

func TestBuildCleanAndParse(t *testing.T) {
	dir := t.TempDir()
	set, err := Build(Spec{Root: dir, Seed: 41, Case: CaseClean})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(set.Media) == 0 {
		t.Fatal("no media")
	}
	if _, err := readManifest(set.Manifest); err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if _, err := readEdits(set.Edits); err != nil {
		t.Fatalf("edits: %v", err)
	}
}
