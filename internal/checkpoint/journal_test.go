package checkpoint

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"mediad/internal/timeline"
)

func completeRec(id int) SegmentRecord {
	return SegmentRecord{
		ID: id, File: "seg-0.mp4", SHA256: "abc", Size: 10,
		StartNS: 0, EndNS: 100, DurationNS: 100,
	}
}

func TestJournalOnlyRecordsCompleteSegments(t *testing.T) {
	j := NewJournal("h264", 41, 900000)
	bad := completeRec(0)
	bad.SHA256 = ""
	if err := j.AddSegment(bad); err == nil || timeline.ExitCode(err) != 4 {
		t.Fatalf("incomplete record must be rejected with storage/internal exit: %v", err)
	}
	if err := j.AddSegment(completeRec(0)); err != nil {
		t.Fatal(err)
	}
	if err := j.AddSegment(completeRec(0)); err == nil {
		t.Fatal("duplicate segment id must fail")
	}
}

func TestSaveLoadRoundTripAndMissingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "journal.json")
	if _, exists, err := Load(path); err != nil || exists {
		t.Fatalf("missing journal should be empty/exists=false: exists=%v err=%v", exists, err)
	}
	j := NewJournal("av1", 41, 900000)
	if err := j.AddSegment(completeRec(2)); err != nil {
		t.Fatal(err)
	}
	j.ManifestFile = "manifest.json"
	j.ManifestSHA256 = "deadbeef"
	j.Completed = true
	if err := j.SaveAtomic(path); err != nil {
		t.Fatal(err)
	}
	got, exists, err := Load(path)
	if err != nil || !exists {
		t.Fatalf("load exists=%v err=%v", exists, err)
	}
	if got.Codec != "av1" || len(got.Segments) != 1 || got.ManifestSHA256 != "deadbeef" {
		t.Fatalf("bad round trip: %+v", got)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "nested", ".tmp-*"))
	if len(matches) != 0 {
		t.Fatalf("temporary files left behind: %v", matches)
	}
}

func TestCorruptJournalFailsClearly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := Load(path)
	if !errors.Is(err, ErrCorruptCheckpoint) {
		t.Fatalf("err=%v must wrap ErrCorruptCheckpoint", err)
	}
	if code := timeline.ExitCode(err); code != 4 {
		t.Fatalf("exit=%d want 4", code)
	}
}

func TestSafePathRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	bad := []string{"../escape", "/abs/path", "a/../../escape", ".."}
	for _, name := range bad {
		if _, err := SafePath(root, name); err == nil {
			t.Fatalf("unsafe name %q accepted", name)
		}
	}
	full, err := SafePath(root, filepath.Join("segments", "seg-0.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	if dir := filepath.Dir(full); dir != filepath.Join(root, "segments") {
		t.Fatalf("unexpected full path %q", full)
	}
}

func TestVerifyFilesDetectsTampering(t *testing.T) {
	root := t.TempDir()
	rec := completeRec(0)
	rec.File = "seg-0.bin"
	full, err := SafePath(root, rec.File)
	if err != nil {
		t.Fatal(err)
	}
	if err := AtomicWriteFile(full, []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	sum, size, err := HashFile(full)
	if err != nil {
		t.Fatal(err)
	}
	rec.SHA256 = sum
	rec.Size = size
	j := NewJournal("h264", 1, 1)
	if err := j.AddSegment(rec); err != nil {
		t.Fatal(err)
	}
	if err := j.VerifyFiles(root); err != nil {
		t.Fatalf("valid file should verify: %v", err)
	}
	if err := os.WriteFile(full, []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := j.VerifyFiles(root); !errors.Is(err, ErrCorruptCheckpoint) {
		t.Fatalf("tamper err=%v want corrupt checkpoint", err)
	}
}
