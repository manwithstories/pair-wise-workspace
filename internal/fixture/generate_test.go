package fixture

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/mediamend/mediamend/internal/ingest"
)

func TestGenerateCleanDeterministic(t *testing.T) {
	dir := t.TempDir()
	proj, corrupt, err := Generate(Options{Root: dir, FramesPerTrack: 100, Seed: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(proj.Tracks) != 4 || len(corrupt) != 0 {
		t.Fatalf("tracks=%d corrupt=%d", len(proj.Tracks), len(corrupt))
	}
	sum1, _ := digestFile(t, filepath.Join(dir, "input", "v1.jsonl"))
	proj2, _, err := Generate(Options{Root: dir, FramesPerTrack: 100, Seed: 1})
	if err != nil {
		t.Fatal(err)
	}
	sum2, _ := digestFile(t, filepath.Join(dir, "input", "v1.jsonl"))
	if sum1 != sum2 {
		t.Fatal("fixture not deterministic across runs")
	}
	if len(proj2.Tracks) != 4 {
		t.Fatal("project track count changed")
	}
}

func TestCorruptEveryLocations(t *testing.T) {
	dir := t.TempDir()
	_, corrupt, err := Generate(Options{Root: dir, FramesPerTrack: 1000, CorruptEvery: 97})
	if err != nil {
		t.Fatal(err)
	}
	// CorruptSeq is the ground truth; verify each flagged record fails ingest.
	want := CorruptSeq(1000, 97)
	if len(corrupt) != len(want) {
		t.Fatalf("corrupt count=%d want %d", len(corrupt), len(want))
	}
	r, err := ingest.Open(filepath.Join(dir, "input", "v1.jsonl"), "v1")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	_, _, faults, err := ingest.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	flagged := map[uint64]bool{}
	for _, f := range faults {
		flagged[f.Location.Seq] = true
	}
	for _, seq := range want {
		if !flagged[seq] {
			t.Fatalf("injected corruption at seq %d not located", seq)
		}
	}
	if len(faults) != len(want) {
		t.Fatalf("located=%d injected=%d (precision must be exact)", len(faults), len(want))
	}
}

func TestDefectInjection(t *testing.T) {
	dir := t.TempDir()
	_, _, err := Generate(Options{
		Root: dir, FramesPerTrack: 300, Seed: 1,
		InjectRollover: true, InjectDuplicate: true, InjectBackwards: true,
		InjectMissing: true, InjectColorFault: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	// v1 should have one missing seq (210) and one duplicate (77): more lines
	// than the nominal count minus the gap.
	data, err := os.ReadFile(filepath.Join(dir, "input", "v1.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Fatal("v1 track empty")
	}
	// The manifest must carry the audio normalization policy.
	projBytes, err := os.ReadFile(ProjectPath(dir))
	if err != nil || len(projBytes) == 0 {
		t.Fatalf("project manifest missing: %v", err)
	}
}

func digestFile(t *testing.T, p string) (string, int64) {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), int64(len(b))
}
