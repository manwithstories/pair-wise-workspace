package pipeline

import (
	"os"
	"path/filepath"
	"testing"

	"tsqc/internal/fixture"
)

func demoOptions(dir string, seed int64, faults []string) RunOptions {
	return RunOptions{
		Seed: seed, Faults: faults,
		State: filepath.Join(dir, "state"), Out: filepath.Join(dir, "out"),
		IDRs: 8, GOP: 4, Audio: 3,
	}
}

func TestRunAllFaultsMeetsAcceptance(t *testing.T) {
	dir := t.TempDir()
	rep, err := Run(demoOptions(dir, 41, fixture.AllFaults))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Fatal {
		t.Fatal("stream reported unplayable")
	}
	if rep.Roundtrip < 100 || rep.KeyframeHitRate < 99 {
		t.Fatalf("roundtrip=%.2f hit=%.2f", rep.Roundtrip, rep.KeyframeHitRate)
	}
	if rep.Segments != 8 {
		t.Fatalf("segments=%d", rep.Segments)
	}
	if rep.Recovered < 1 || rep.Unrecoverable < 1 {
		t.Fatalf("gaps recovered=%d unrecoverable=%d", rep.Recovered, rep.Unrecoverable)
	}
	for i, d := range rep.Digests {
		if d == "" {
			t.Fatalf("segment %d has empty digest", i)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "out", "manifest.json")); err != nil {
		t.Fatalf("manifest missing: %v", err)
	}
}

func TestRunIsDeterministic(t *testing.T) {
	a, err := Run(demoOptions(t.TempDir(), 41, fixture.AllFaults))
	if err != nil {
		t.Fatal(err)
	}
	b, err := Run(demoOptions(t.TempDir(), 41, fixture.AllFaults))
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Digests) != len(b.Digests) {
		t.Fatal("segment count differs")
	}
	for i := range a.Digests {
		if a.Digests[i] != b.Digests[i] {
			t.Fatalf("segment %d digest differs", i)
		}
	}
	c, err := Run(demoOptions(t.TempDir(), 42, fixture.AllFaults))
	if err != nil {
		t.Fatal(err)
	}
	if sameDigests(a.Digests, c.Digests) {
		t.Fatal("different seeds produced identical output")
	}
}

func TestRunResumeDoesNotDuplicate(t *testing.T) {
	dir := t.TempDir()
	opt := demoOptions(dir, 41, fixture.AllFaults)
	first, err := Run(opt)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Run(opt)
	if err != nil {
		t.Fatal(err)
	}
	if second.ResumedSegments != first.Segments {
		t.Fatalf("resumed=%d want %d", second.ResumedSegments, first.Segments)
	}
	if second.CheckpointSaved != 0 {
		t.Fatalf("re-committed %d segments", second.CheckpointSaved)
	}
	if second.CacheMisses != 0 {
		t.Fatalf("cache misses on rerun=%d", second.CacheMisses)
	}
	if !sameDigests(first.Digests, second.Digests) {
		t.Fatal("rerun digests drifted")
	}
}

func TestRunRecoversFromCorruptCheckpoint(t *testing.T) {
	dir := t.TempDir()
	opt := demoOptions(dir, 41, nil)
	if _, err := os.Stat(filepath.Join(opt.State)); err != nil {
		os.MkdirAll(opt.State, 0o755)
	}
	if err := os.WriteFile(CheckpointPath(opt.State), []byte("{broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	rep, err := Run(opt)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Notes) == 0 {
		t.Fatal("corrupt checkpoint not reported")
	}
	if rep.CheckpointSaved == 0 {
		t.Fatal("work not recomputed after corruption")
	}
}

func TestVerifyDirDetectsTamper(t *testing.T) {
	dir := t.TempDir()
	opt := demoOptions(dir, 41, fixture.AllFaults)
	if _, err := Run(opt); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyDir(opt.Out); err != nil {
		t.Fatalf("clean verify failed: %v", err)
	}
	path := filepath.Join(opt.Out, "seg-00.ts")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)/2] ^= 0xff
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyDir(opt.Out); err == nil {
		t.Fatal("tampered segment passed verification")
	}
}

func sameDigests(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
