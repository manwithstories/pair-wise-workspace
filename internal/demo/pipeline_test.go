package demo

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mediasync/internal/clock"
	"mediasync/internal/fixture"
)

func testTB() clock.Timebase {
	return clock.Timebase{FPS: clock.Rational{Num: 30000, Den: 1001}}
}

func allFaults() map[string]bool {
	return map[string]bool{
		fixture.FaultDuplicate: true,
		fixture.FaultReorder:   true,
		fixture.FaultBurstLoss: true,
	}
}

func committedHashes(t *testing.T, dir string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "commit.out"))
	if err != nil {
		t.Fatal(err)
	}
	var stored struct {
		Hashes []string `json:"hashes"`
	}
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	return stored.Hashes
}

func hasLine(lines []string, want string) bool {
	for _, l := range lines {
		if l == want {
			return true
		}
	}
	return false
}

func TestPipelineCrashRecoveryAndIdempotentRerun(t *testing.T) {
	dir := t.TempDir()
	tb := testTB()
	bit := []int{800, 1600, 3200}

	m1, code1 := Pipeline(42, tb, "30000/1001", bit, allFaults(), "commit", dir)
	if code1 != ExitOK {
		t.Fatalf("first run exit = %d: %v", code1, m1)
	}
	for _, want := range []string{
		"recovered=true", "rollback_count=1", "segment_hash_mismatch=0",
		"segments=100", "keyframe_hit_rate=0.9500", "rejected_records=3",
	} {
		if !hasLine(m1, want) {
			t.Fatalf("first run missing %q: %v", want, m1)
		}
	}
	if !strings.HasPrefix(m1[0], "roundtrip_rate=0.99") {
		t.Fatalf("roundtrip line: %q", m1[0])
	}
	h1 := committedHashes(t, dir)

	// Rerun with the same seed: per-segment SHA-256 must be identical.
	m2, code2 := Pipeline(42, tb, "30000/1001", bit, allFaults(), "commit", dir)
	if code2 != ExitOK {
		t.Fatalf("rerun exit = %d: %v", code2, m2)
	}
	h2 := committedHashes(t, dir)
	if len(h1) != len(h2) {
		t.Fatalf("hash count changed: %d vs %d", len(h1), len(h2))
	}
	for i := range h1 {
		if h1[i] != h2[i] {
			t.Fatalf("segment %d hash changed across rerun", i)
		}
	}
}

func TestPipelineHashMismatchExit3(t *testing.T) {
	dir := t.TempDir()
	tb := testTB()
	bit := []int{800, 1600, 3200}
	if _, code := Pipeline(42, tb, "30000/1001", bit, allFaults(), "", dir); code != ExitOK {
		t.Fatalf("setup run exit = %d", code)
	}
	// Tamper with the committed artifact: verification must refuse (exit 3)
	// and locate the segment.
	p := filepath.Join(dir, "commit.out")
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	doc["hashes"].([]any)[7] = strings.Repeat("0", 64)
	tampered, _ := json.Marshal(doc)
	if err := os.WriteFile(p, tampered, 0o644); err != nil {
		t.Fatal(err)
	}
	m, code := Pipeline(42, tb, "30000/1001", bit, allFaults(), "", dir)
	if code != ExitRecovery {
		t.Fatalf("tampered run exit = %d, want 3", code)
	}
	if !hasLine(m, "segment_hash_mismatch=1") {
		t.Fatalf("metrics = %v", m)
	}
}
