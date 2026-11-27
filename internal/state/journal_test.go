package state

import (
	"os"
	"path/filepath"
	"testing"
)

func sampleFP() Fingerprint {
	return Fingerprint{InputHash: "in1", ConfigHash: "cfg1", AdapterVersion: "ffmpeg_v1"}
}

func writeTemp(t *testing.T, j *Journal, name, content string) string {
	t.Helper()
	tmp := j.TempPath(name)
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return tmp
}

func TestCommitAndReopen(t *testing.T) {
	dir := t.TempDir()
	j, err := Open(dir, sampleFP())
	if err != nil {
		t.Fatal(err)
	}
	writeTemp(t, j, "v0c0.m4s", "segment-bytes")
	rec := SegmentRecord{ID: "v0c0", Variant: 0, CutIndex: 0, Path: "v0c0.m4s",
		TargetBPS: 900000, StartTicks: 0, EndTicks: 90000}
	committed, err := j.Commit(rec, "")
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if committed.SHA256 == "" {
		t.Fatal("commit must record observed hash")
	}
	// Temp file gone, final file present.
	if _, err := os.Stat(j.TempPath("v0c0.m4s")); !os.IsNotExist(err) {
		t.Error("temp file not removed after commit")
	}
	if _, err := os.Stat(filepath.Join(dir, "v0c0.m4s")); err != nil {
		t.Errorf("final segment missing: %v", err)
	}
	// Reopen: checkpoint is reused.
	j2, err := Open(dir, sampleFP())
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := j2.Lookup(0, 0); !ok || got.SHA256 != committed.SHA256 {
		t.Errorf("checkpoint not recovered: %+v ok=%v", got, ok)
	}
	reused, faults := j2.Verify()
	if reused != 1 || len(faults) != 0 {
		t.Errorf("clean verify: reused=%d faults=%d", reused, len(faults))
	}
}

func TestVerifyDetectsMissingSegment(t *testing.T) {
	dir := t.TempDir()
	j, _ := Open(dir, sampleFP())
	writeTemp(t, j, "v0c0.m4s", "abc")
	if _, err := j.Commit(SegmentRecord{Variant: 0, CutIndex: 0, Path: "v0c0.m4s"}, ""); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "v0c0.m4s")); err != nil {
		t.Fatal(err)
	}
	j2, _ := Open(dir, sampleFP())
	reused, faults := j2.Verify()
	if reused != 0 {
		t.Errorf("missing segment must not be reused, got %d", reused)
	}
	if len(faults) != 1 || faults[0].Code != "E_MISSING_SEGMENT" {
		t.Errorf("want one missing-segment fault, got %+v", faults)
	}
	if _, ok := j2.Lookup(0, 0); ok {
		t.Error("invalid checkpoint must be dropped")
	}
}

func TestVerifyDetectsHashCorruption(t *testing.T) {
	dir := t.TempDir()
	j, _ := Open(dir, sampleFP())
	writeTemp(t, j, "v0c0.m4s", "original")
	if _, err := j.Commit(SegmentRecord{Variant: 0, CutIndex: 0, Path: "v0c0.m4s"}, ""); err != nil {
		t.Fatal(err)
	}
	// Tamper with the committed file without updating the journal.
	if err := os.WriteFile(filepath.Join(dir, "v0c0.m4s"), []byte("tampered!!!"), 0o644); err != nil {
		t.Fatal(err)
	}
	j2, _ := Open(dir, sampleFP())
	reused, faults := j2.Verify()
	if reused != 0 || len(faults) != 1 || faults[0].Code != "E_HASH_MISMATCH" {
		t.Errorf("corruption not isolated: reused=%d faults=%+v", reused, faults)
	}
	// The corrupt file is removed so a fresh encode can take its place.
	if _, err := os.Stat(filepath.Join(dir, "v0c0.m4s")); !os.IsNotExist(err) {
		t.Error("corrupt segment should be quarantined by removal")
	}
}

func TestFingerprintMismatchStartsFresh(t *testing.T) {
	dir := t.TempDir()
	j, _ := Open(dir, sampleFP())
	writeTemp(t, j, "v0c0.m4s", "x")
	if _, err := j.Commit(SegmentRecord{Variant: 0, CutIndex: 0, Path: "v0c0.m4s"}, ""); err != nil {
		t.Fatal(err)
	}
	changed := sampleFP()
	changed.ConfigHash = "cfg2"
	j2, err := Open(dir, changed)
	if err != nil {
		t.Fatal(err)
	}
	if len(j2.Segments) != 0 {
		t.Error("changed fingerprint must not reuse old checkpoints")
	}
	if j2.Fingerprint.ConfigHash != "cfg2" {
		t.Error("new fingerprint not adopted")
	}
}

func TestCommitRejectsWrongHash(t *testing.T) {
	dir := t.TempDir()
	j, _ := Open(dir, sampleFP())
	writeTemp(t, j, "v0c0.m4s", "data")
	_, err := j.Commit(SegmentRecord{Variant: 0, CutIndex: 0, Path: "v0c0.m4s"}, "deadbeef")
	if err == nil {
		t.Fatal("expected hash mismatch error")
	}
	// Temp file cleaned up after rejection.
	if _, err := os.Stat(j.TempPath("v0c0.m4s")); !os.IsNotExist(err) {
		t.Error("rejected temp file must be removed")
	}
}

func TestReusedCountAcrossRecovery(t *testing.T) {
	dir := t.TempDir()
	j, _ := Open(dir, sampleFP())
	for i := 0; i < 3; i++ {
		name := "v0c" + string(rune('0'+i)) + ".m4s"
		writeTemp(t, j, name, "content")
		if _, err := j.Commit(SegmentRecord{Variant: 0, CutIndex: i, Path: name}, ""); err != nil {
			t.Fatal(err)
		}
	}
	j2, _ := Open(dir, sampleFP())
	reused, _ := j2.Verify()
	if reused != 3 || j2.Reused != 3 {
		t.Errorf("reused = %d, want 3", reused)
	}
}
