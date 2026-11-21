package verify

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"mediamerge/internal/checksum"
	"mediamerge/internal/demux"
	"mediamerge/internal/fixture"
	"mediamerge/internal/mapping"
	"mediamerge/internal/model"
	"mediamerge/internal/txn"
)

func buildWorkdir(t *testing.T, interruptAfter int) string {
	t.Helper()
	dir := t.TempDir()
	in := filepath.Join(dir, "input.mp4")
	if err := os.WriteFile(in, fixture.DefaultPlan(3, 16).Build(), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := demux.Open(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	plan, err := mapping.Apply(r.Tracks, mapping.DefaultRules(r.Tracks))
	if err != nil {
		t.Fatal(err)
	}
	_, err = txn.Run(context.Background(), dir, r, plan, txn.Options{Queue: 2, InterruptAfter: interruptAfter})
	if interruptAfter < 0 && err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestVerifyCompleteRun(t *testing.T) {
	dir := buildWorkdir(t, -1)
	rep, err := Run(context.Background(), dir)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !rep.SampleMatch || rep.Tracks != 3 || rep.Samples != 48 || !rep.Final {
		t.Fatalf("report=%+v", rep)
	}
	inputSHA, _, _ := checksum.File(filepath.Join(dir, "input.mp4"))
	if rep.InputSHA != inputSHA || len(rep.OutputSHA) != 64 {
		t.Fatalf("digests=%+v", rep)
	}
}

func TestVerifyRejectsIncompleteRun(t *testing.T) {
	dir := buildWorkdir(t, 1)
	_, err := Run(context.Background(), dir)
	if err == nil {
		t.Fatal("verify accepted an incomplete run")
	}
	pe, ok := err.(*model.Error)
	if !ok || pe.Code != model.CodeJournal {
		t.Fatalf("err=%v", err)
	}
}

func TestVerifyDetectsPayloadCorruption(t *testing.T) {
	dir := buildWorkdir(t, -1)
	out := filepath.Join(dir, "output.mp4")
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)/2] ^= 0x01
	if err := os.WriteFile(out, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), dir); err == nil {
		t.Fatal("verify accepted a corrupted output")
	}
}

func TestEnsureDirRejectsSymlink(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := EnsureDir(link); err == nil {
		t.Fatal("symlinked work directory accepted")
	}
}
