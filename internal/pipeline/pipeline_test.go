package pipeline

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"

	"relay/internal/txn"
)

func sha256Sum(p []byte) [32]byte { return sha256.Sum256(p) }
func osMkdirAll(p string) error   { return os.MkdirAll(p, 0o755) }
func osWriteFile(p string, d []byte) error {
	return os.WriteFile(p, d, 0o600)
}
func dirOf(p string) string { return filepath.Dir(p) }

func TestRunHealthyAndDeterministic(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()
	a, err := Run(Config{WorkDir: dirA})
	if err != nil {
		t.Fatal(err)
	}
	b, err := Run(Config{WorkDir: dirB})
	if err != nil {
		t.Fatal(err)
	}
	if a.ExitCode != 0 || b.ExitCode != 0 {
		t.Fatalf("exit codes: a=%d b=%d", a.ExitCode, b.ExitCode)
	}
	if a.Report.Samples != 192 {
		t.Fatalf("samples: got %d want 192", a.Report.Samples)
	}
	if a.InputDigest != a.OutputDigest {
		t.Fatal("aggregate input/output digest differs")
	}
	if a.ManifestSum != b.ManifestSum {
		t.Fatalf("manifest not deterministic: %x vs %x", a.ManifestSum, b.ManifestSum)
	}
	if a.InputDigest != b.InputDigest {
		t.Fatal("input digest not deterministic across runs")
	}
	for _, d := range a.Report.TrackDiffs {
		if !d.Preserved {
			t.Fatalf("track %d sample bytes not preserved", d.TrackID)
		}
	}
}

func TestRunChecksumDamageRollsBack(t *testing.T) {
	dir := t.TempDir()
	out, err := Run(Config{WorkDir: dir, Damage: "checksum"})
	if err != nil {
		t.Fatal(err)
	}
	if out.ExitCode != ExitMediaError {
		t.Fatalf("exit: got %d want %d", out.ExitCode, ExitMediaError)
	}
	if !out.Report.ChecksumError || out.Report.RollbackCount != 1 {
		t.Fatalf("expected checksum mismatch + rollback, got %+v", out.Report)
	}
	if _, ok := txn.CurrentCommitID(dir); ok {
		t.Fatal("CURRENT must not be created by a rolled-back run")
	}
	if me := mediaChecksumReported(out); !me {
		t.Fatal("checksum error kind not surfaced")
	}
}

func TestResumeAfterStagedCrashCommitsOnce(t *testing.T) {
	dir := t.TempDir()
	// Baseline healthy run publishes CURRENT and proves normal commit works.
	out, err := Run(Config{WorkDir: dir})
	if err != nil || out.ExitCode != 0 {
		t.Fatalf("baseline run failed: %v exit=%d", err, out.ExitCode)
	}
	first, ok := txn.CurrentCommitID(dir)
	if !ok {
		t.Fatal("baseline CURRENT missing")
	}

	// Simulate a second, interrupted transaction: Begin + staged sample +
	// manifest, but no commit, exactly what --crash-after staged leaves behind.
	stagedID := [32]byte{0xde, 0xad, 0xbe, 0xef}
	w, err := txn.OpenWAL(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Append(txn.RecBegin, stagedID[:]); err != nil {
		t.Fatal(err)
	}
	payload := []byte("interrupted-sample")
	staging := dir + "/staging/track-9/sample-0000000000.bin"
	if err := mkdirFile(staging, payload); err != nil {
		t.Fatal(err)
	}
	rec := txn.StageRecord{
		TrackID: 9, Sequence: 0, Size: uint32(len(payload)),
		RelPath: "staging/track-9/sample-0000000000.bin",
	}
	rec.SHA256 = sha256Sum(payload)
	if _, err := w.Append(txn.RecStage, txn.EncodeStage(rec)); err != nil {
		t.Fatal(err)
	}
	manifest := txn.Manifest{CommitID: stagedID, Entries: []txn.ManifestEntry{{
		TrackID: 9, Sequence: 0, Size: rec.Size, SHA256: rec.SHA256,
	}}}
	if _, err := w.Append(txn.RecManifest, txn.EncodeManifest(manifest)); err != nil {
		t.Fatal(err)
	}
	w.Close()

	rs, err := Resume(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !rs.Summary.Committed || rs.Summary.Replayed != 1 {
		t.Fatalf("resume did not commit the staged txn: %+v", rs.Summary)
	}
	if !rs.Report.Identical {
		t.Fatal("resume must report identical manifest")
	}
	// A second resume must not produce a different result and must keep the
	// same committed boundary and manifest hash (idempotent, single commit).
	rs2, err := Resume(dir)
	if err != nil {
		t.Fatal(err)
	}
	if rs2.Report.CommitCount != 1 || rs2.Report.ManifestSum != rs.Report.ManifestSum {
		t.Fatalf("second resume not idempotent: %+v", rs2.Summary)
	}
	after, _ := txn.CurrentCommitID(dir)
	_ = first
	if after != stagedID {
		t.Fatal("CURRENT should now point at the recovered commit")
	}
}

func mkdirFile(path string, data []byte) error {
	if err := osMkdirAll(dirOf(path)); err != nil {
		return err
	}
	return osWriteFile(path, data)
}

func mediaChecksumReported(out *Outcome) bool { return out.Report.ChecksumError }
