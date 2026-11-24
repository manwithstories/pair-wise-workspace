package txn

import (
	"os"
	"path/filepath"
	"testing"

	"bcast/internal/media"
)

func TestWALAppendAndRead(t *testing.T) {
	out := t.TempDir()
	w, err := OpenWAL(out)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Append(WALEntry{Stage: "scan", Generation: "g1", Status: "ok", Summary: "3"}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := ReadWAL(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Stage != "scan" || entries[0].Summary != "3" {
		t.Fatalf("entries=%+v", entries)
	}
}

func TestWALTornRecordIsLocated(t *testing.T) {
	out := t.TempDir()
	path := filepath.Join(out, "wal.log")
	if err := os.WriteFile(path, []byte(`{"stage":"index"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := ReadWAL(out)
	me, ok := media.AsError(err)
	if !ok || me.ExitCode != media.ExitCorrupt || me.Offset != 0 {
		t.Fatalf("torn WAL err=%v", err)
	}
}

func TestCommitMarkerAndStableSHA(t *testing.T) {
	out := t.TempDir()
	tx, err := Begin(out, "gen-0001")
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.WriteArtifact("manifest.m3u8", []byte("#EXTM3U\n")); err != nil {
		t.Fatal(err)
	}
	if err := tx.WriteArtifact("index.json", []byte("{}\n")); err != nil {
		t.Fatal(err)
	}
	rec, err := tx.Commit()
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.SHA256) != 64 {
		t.Fatalf("sha=%q", rec.SHA256)
	}
	loaded, err, ok := LoadCommit(out)
	if err != nil || !ok {
		t.Fatalf("load commit ok=%v err=%v", ok, err)
	}
	if loaded.SHA256 != rec.SHA256 {
		t.Fatalf("marker sha=%s want %s", loaded.SHA256, rec.SHA256)
	}
	if _, err := os.Stat(filepath.Join(out, "committed", "gen-0001", "SHA256SUMS")); err != nil {
		t.Fatal(err)
	}

	out2 := t.TempDir()
	tx2, err := Begin(out2, "gen-0001")
	if err != nil {
		t.Fatal(err)
	}
	_ = tx2.WriteArtifact("manifest.m3u8", []byte("#EXTM3U\n"))
	_ = tx2.WriteArtifact("index.json", []byte("{}\n"))
	rec2, err := tx2.Commit()
	if err != nil {
		t.Fatal(err)
	}
	if rec2.SHA256 != rec.SHA256 {
		t.Fatalf("deterministic SHA changed: %s vs %s", rec2.SHA256, rec.SHA256)
	}
}

func TestRollbackStagingGeneration(t *testing.T) {
	out := t.TempDir()
	tx, err := Begin(out, "gen-bad")
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.WriteArtifact("partial", []byte("half")); err != nil {
		t.Fatal(err)
	}
	if err := tx.Close(); err != nil {
		t.Fatal(err)
	}
	gens, err := StagingGenerations(out)
	if err != nil || len(gens) != 1 {
		t.Fatalf("gens=%v err=%v", gens, err)
	}
	n, err := RollbackStaging(out)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("rollback=%d want 1", n)
	}
	if _, err := os.Stat(filepath.Join(out, "staging", "gen-bad")); !os.IsNotExist(err) {
		t.Fatalf("staging dir remains: %v", err)
	}
}

func TestVerifyCommitDetectsTamperedArtifact(t *testing.T) {
	out := t.TempDir()
	tx, err := Begin(out, "gen-tamper")
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.WriteArtifact("report.json", []byte("{\"ok\":true}\n")); err != nil {
		t.Fatal(err)
	}
	rec, err := tx.Commit()
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyCommit(out, rec); err != nil {
		t.Fatalf("fresh commit verification: %v", err)
	}
	artifact := filepath.Join(out, "committed", rec.Generation, "report.json")
	if err := os.WriteFile(artifact, []byte("{\"ok\":false}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := VerifyCommit(out, rec); err == nil {
		t.Fatal("expected tamper to fail SHA verification")
	}
}

func TestWriteFileAtomicMapsDiskPathError(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "afile")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := WriteFileAtomic(filepath.Join(blocker, "child"), []byte("data"))
	if media.ExitCodeOf(err) != media.ExitIO {
		t.Fatalf("err=%v exit=%d want I/O 3", err, media.ExitCodeOf(err))
	}
}
