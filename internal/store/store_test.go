package store

import (
	"os"
	"path/filepath"
	"testing"

	"pairflow/internal/media"
)

func newJail(t *testing.T) *Jail {
	t.Helper()
	dir := t.TempDir()
	// The jail root itself must be a real directory.
	j, err := OpenJail(dir)
	if err != nil {
		t.Fatalf("open jail: %v", err)
	}
	return j
}

func TestJailRejectsEscapesAndSymlinks(t *testing.T) {
	j := newJail(t)
	cases := []struct{ rel, code string }{
		{"../escape", "store.escape"},
		{"a/../../escape", "store.escape"},
		{"/etc/passwd", "store.absolute_path"},
		{"", "store.empty_path"},
	}
	for _, c := range cases {
		if _, err := j.Resolve(c.rel); err == nil || err.Code != c.code {
			t.Fatalf("%q: want %s, got %v", c.rel, c.code, err)
		}
	}
	// A symlinked target is refused even inside the jail.
	if err := os.Symlink(filepath.Join(j.Root(), "real.txt"), filepath.Join(j.Root(), "link.txt")); err != nil {
		t.Skip("symlink unsupported")
	}
	if err := os.WriteFile(filepath.Join(j.Root(), "real.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Resolve("link.txt"); err == nil || err.Code != "store.symlink" {
		t.Fatalf("symlink accepted: %v", err)
	}
}

func TestWALAppendAndReload(t *testing.T) {
	j := newJail(t)
	w, err := OpenWAL(j.Root())
	if err != nil {
		t.Fatalf("open wal: %v", err)
	}
	if err := w.Begin("decode", "abc"); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := w.Done("decode", "abc", []Artifact{{Path: "out/decode/x", Size: 3, SHA: HashBytes([]byte("xyz"))}}); err != nil {
		t.Fatalf("done: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	w2, err := OpenWAL(j.Root())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer w2.Close()
	if got := w2.DoneStages("abc"); len(got) != 1 || got[0] != "decode" {
		t.Fatalf("done stages %v", got)
	}
	if r, ok := w2.LastDone("abc"); !ok || r.Artifacts[0].SHA != HashBytes([]byte("xyz")) {
		t.Fatalf("last done %+v", r)
	}
	if err := w2.Begin("audio", "abc"); err != nil {
		t.Fatalf("resumed begin: %v", err)
	}
	if w2.seq != 3 {
		t.Fatalf("sequence did not continue: %d", w2.seq)
	}
}

func TestWALRejectsCorruptLine(t *testing.T) {
	j := newJail(t)
	if err := os.WriteFile(filepath.Join(j.Root(), walName), []byte("{not json}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenWAL(j.Root()); err == nil || err.Class != media.ClassIntegrity {
		t.Fatalf("corrupt journal must be an integrity error, got %v", err)
	}
}

func TestStageCommitAndRollback(t *testing.T) {
	j := newJail(t)
	st, err := NewStageStager(j, "audio")
	if err != nil {
		t.Fatalf("stager: %v", err)
	}
	a, err := st.WriteStaged("mix.wav", []byte("RIFFdata"), "wav")
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	// Before commit the artifact exists only under staging.
	if _, serr := os.Stat(filepath.Join(j.Root(), StagingDir, "audio", "mix.wav")); serr != nil {
		t.Fatal("staged file missing")
	}
	if _, oerr := os.Stat(filepath.Join(j.Root(), a.Path)); oerr == nil {
		t.Fatal("artifact visible before commit")
	}
	arts, err := st.Commit()
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if _, oerr := os.Stat(filepath.Join(j.Root(), a.Path)); oerr != nil {
		t.Fatal("artifact missing after commit")
	}
	if err := j.VerifyArtifacts(arts); err != nil {
		t.Fatalf("verify: %v", err)
	}
	// Tampering must be detected.
	if err := os.WriteFile(filepath.Join(j.Root(), a.Path), []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := j.VerifyArtifacts(arts); err == nil || err.Class != media.ClassIntegrity {
		t.Fatalf("tampered artifact not detected: %v", err)
	}

	// Rollback removes staging and an uncommitted out stage, leaving no trace.
	st2, err := NewStageStager(j, "compose")
	if err != nil {
		t.Fatalf("stager2: %v", err)
	}
	if _, err := st2.WriteStaged("frame.png", []byte("png"), "png"); err != nil {
		t.Fatal(err)
	}
	if err := st2.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if _, err := os.Stat(filepath.Join(j.Root(), StagingDir, "compose")); !os.IsNotExist(err) {
		t.Fatal("staging survived rollback")
	}
	if _, err := os.Stat(filepath.Join(j.Root(), OutDir, "compose")); !os.IsNotExist(err) {
		t.Fatal("uncommitted out stage survived rollback")
	}
	// The previously committed audio stage must be untouched.
	if _, err := os.Stat(filepath.Join(j.Root(), a.Path)); err != nil {
		t.Fatal("rollback damaged another stage")
	}
}

func TestManifestRoundTrip(t *testing.T) {
	j := newJail(t)
	m := &ManifestDoc{Version: 1, Timeline: "deadbeef", Seed: 17, Faults: "mixed",
		Stages: []StageDoc{{Name: "audio", Artifacts: []Artifact{{Path: "out/audio/mix.wav", Size: 4, SHA: HashBytes([]byte("abcd")), Kind: "wav"}}}}}
	if err := j.WriteManifest(m); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	got, ok, err := j.ReadManifest()
	if err != nil || !ok {
		t.Fatalf("read manifest: %v ok=%v", err, ok)
	}
	if got.Timeline != "deadbeef" || got.Seed != 17 || len(got.Stages) != 1 {
		t.Fatalf("manifest mismatch: %+v", got)
	}
}

func TestOpenJailRejectsSymlinkRoot(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symlink unsupported")
	}
	if _, err := OpenJail(link); err == nil || err.Code != "store.root_symlink" {
		t.Fatalf("symlinked root accepted: %v", err)
	}
}
