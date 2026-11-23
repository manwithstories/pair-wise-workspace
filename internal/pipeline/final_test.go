package pipeline

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFinalManifestContent pins what the final artefact contains: the counts
// an editor relies on and the provenance summary, with no work-directory
// paths leaking in.
func TestFinalManifestContent(t *testing.T) {
	dir := t.TempDir()
	res, err := Run(Config{
		Seed: 7, Segments: 3, SegmentDurNS: 2_000_000_000,
		PacketRate: 50, WorkDir: dir, Cameras: []string{"cam0"},
		Faults: Faults{Drop: true},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "final.json"))
	if err != nil {
		t.Fatalf("read final: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("final manifest is not valid JSON: %v", err)
	}
	if doc["seed"] != float64(7) {
		t.Fatalf("seed = %v", doc["seed"])
	}
	if doc["segments"] != float64(3) {
		t.Fatalf("segments = %v", doc["segments"])
	}
	if doc["trace"] != res.TraceSummary {
		t.Fatalf("trace summary mismatch")
	}
	if string(raw) == "" || filepath.Base(dir) == "" {
		t.Fatal("empty manifest")
	}
	// A work-directory path inside the artefact would make reruns differ.
	if strings.Contains(string(raw), dir) {
		t.Fatalf("final manifest leaks the work directory path")
	}
}
