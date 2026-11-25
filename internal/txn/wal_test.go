package txn_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"mediacheck/internal/media"
	"mediacheck/internal/txn"
)

func TestSafeJoinRejectsTraversalAndAbsolute(t *testing.T) {
	root := t.TempDir()
	if _, err := txn.SafeJoin(root, "normal/fmp4.mp4"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"../escape", "a/../../escape", "/etc/passwd"} {
		_, err := txn.SafeJoin(root, bad)
		var me *media.Error
		if err == nil || !errors.As(err, &me) || me.Kind != media.KindPathTraversal {
			t.Fatalf("bad path %q err=%v", bad, err)
		}
	}
}

func TestWALRecoveryReusesOnlyCommittedStages(t *testing.T) {
	root := t.TempDir()
	w, err := txn.Open(root, "tx-1")
	if err != nil {
		t.Fatal(err)
	}
	p, err := w.Begin("audio", "audio.pcm", 180000)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("deterministic audio")
	if err := p.Write(data); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Commit(); err != nil {
		t.Fatal(err)
	}

	uncommitted, err := w.Begin("mux", "fmp4.mp4", 180000)
	if err != nil {
		t.Fatal(err)
	}
	if err := uncommitted.Write([]byte("prepared but not committed")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	rec, err := txn.Recover(root, "tx-1")
	if err != nil {
		t.Fatal(err)
	}
	if !rec.Has("audio") || rec.Has("mux") {
		t.Fatalf("completed=%v", rec.Completed)
	}
	stages := rec.Stages("audio", "mux")
	if len(stages) != 1 || stages[0] != "audio" {
		t.Fatalf("stages=%v", stages)
	}
}

func TestRecoveryDiscardsHashMismatch(t *testing.T) {
	root := t.TempDir()
	w, err := txn.Open(root, "tx-2")
	if err != nil {
		t.Fatal(err)
	}
	p, err := w.Begin("mux", "fmp4.mp4", 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Write([]byte("original")); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	path, err := txn.SafeJoin(root, "fmp4.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec, err := txn.Recover(root, "tx-2")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Has("mux") {
		t.Fatal("tampered stage must not be reused")
	}
	if len(rec.Invalid) != 1 {
		t.Fatalf("invalid=%v", rec.Invalid)
	}
}

func TestCleanTemporaryArtifacts(t *testing.T) {
	root := t.TempDir()
	tmp := filepath.Join(root, "stage.tmp")
	if err := os.WriteFile(tmp, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := txn.CleanTemporaryArtifacts(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatalf("tmp exists or stat err=%v", err)
	}
}
