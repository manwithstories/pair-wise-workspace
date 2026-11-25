package recover

import (
	"os"
	"path/filepath"
	"testing"

	"fieldcut/internal/clock"
)

func TestJournalCommitReuseAndFailure(t *testing.T) {
	dir := t.TempDir()
	j, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if j.IsCommitted("cut", "hash-a") {
		t.Fatal("empty journal cannot be committed")
	}
	if err := j.Commit("cut", "hash-a", map[string]string{"seg-0.mp4": "sha"}); err != nil {
		t.Fatal(err)
	}
	if err := j.Fail("ingest", "hash-b", &clock.Error{
		Code: clock.CodeTruncatedMP4, TrackID: 3, StartNs: 10, EndNs: 20,
		Message: "truncated moof/mdat",
	}); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reopened.IsCommitted("cut", "hash-a") {
		t.Fatal("committed stage was not recovered")
	}
	if reopened.IsCommitted("cut", "different-input") {
		t.Fatal("stage must rerun when input hash changes")
	}
	reused := reopened.ReusedNodes(map[string]string{"cut": "hash-a", "manifest": "h"})
	if len(reused) != 1 || reused[0] != "cut" {
		t.Fatalf("reused=%v", reused)
	}
	rec, ok := reopened.Record("ingest")
	if !ok || rec.Error == nil || rec.Error.Code != clock.CodeTruncatedMP4 || rec.Error.TrackID != 3 {
		t.Fatalf("failure record missing truncation: %#v", rec)
	}
}

func TestAtomicWriteFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "manifest.json")
	if err := AtomicWriteFile(path, []byte("{}\n")); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != "{}\n" {
		t.Fatalf("read=%q err=%v", b, err)
	}
}
