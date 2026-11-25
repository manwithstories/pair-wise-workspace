package main

import (
	"os"
	"path/filepath"
	"testing"

	"archivectl/internal/manifest"
	"archivectl/internal/synth"
	"archivectl/internal/timeline"
)

func TestEndToEndAcceptanceContract(t *testing.T) {
	dir := t.TempDir()
	files, err := synth.Generate(dir, 41)
	if err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(dir, "out.mp4")
	list := filepath.Join(dir, "list.csv")
	m, _, err := executeRecover(recoverOptions{
		In: files.Normal, Out: out, Manifest: list,
		TargetKBPS: 1200, Window: 64,
	})
	if err != nil {
		t.Fatal(err)
	}
	if m.code != 0 {
		t.Fatalf("normal exit code=%d", m.code)
	}
	if !m.bandwidth.WithinTolerance(3) {
		t.Fatalf("deviation=%.3f%% exceeds 3%%", m.bandwidth.DeviationPct)
	}
	if m.manifestMaxErrorMS > 1 || m.payloadMismatch != 0 {
		t.Fatalf("maxError=%f mismatch=%d", m.manifestMaxErrorMS, m.payloadMismatch)
	}
	if code := runVerify([]string{"-source", files.Normal, "-output", out, "-manifest", list}); code != 0 {
		t.Fatalf("verify code=%d want 0", code)
	}

	partialOut := filepath.Join(dir, "partial.mp4")
	partialList := filepath.Join(dir, "partial.csv")
	journal := filepath.Join(dir, "run.journal")
	opts := recoverOptions{
		In: files.BurstRTP, Out: partialOut, Manifest: partialList, Journal: journal,
		TargetKBPS: 1200, Window: 64,
	}
	pm, j, err := executeRecover(opts)
	if err != nil {
		t.Fatal(err)
	}
	if pm.code != 2 || pm.gaps != 3 || j.GapLedger[0].Start != 113 || j.GapLedger[0].End != 115 {
		t.Fatalf("partial metrics=%+v journal=%+v", pm, j.GapLedger)
	}
	firstOut, firstList := hashFile(t, partialOut), hashFile(t, partialList)

	resumeOpts := opts
	resumeOpts.Resume = true
	rm, _, err := executeRecover(resumeOpts)
	if err != nil {
		t.Fatal(err)
	}
	if rm.code != 2 {
		t.Fatalf("resume code=%d want 2", rm.code)
	}
	if got := hashFile(t, partialOut); got != firstOut {
		t.Fatalf("output hash changed after resume: %s != %s", got, firstOut)
	}
	if got := hashFile(t, partialList); got != firstList {
		t.Fatalf("manifest hash changed after resume: %s != %s", got, firstList)
	}

	badOut := filepath.Join(dir, "bad.mp4")
	badList := filepath.Join(dir, "bad.csv")
	_, _, err = executeRecover(recoverOptions{
		In: files.TruncatedMP4, Out: badOut, Manifest: badList,
		TargetKBPS: 1200, Window: 64,
	})
	if timeline.ExitCode(err) != 1 {
		t.Fatalf("truncated exit=%d want 1, err=%v", timeline.ExitCode(err), err)
	}
	if _, err := os.Stat(badOut); !os.IsNotExist(err) {
		t.Fatalf("truncated recovery left output: stat err=%v", err)
	}
}

func hashFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return manifest.HashBytes(b)
}
