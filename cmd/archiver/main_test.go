package main

import (
	"os"
	"path/filepath"
	"testing"

	"archivectl/internal/scan"
)

func TestExitCodeArgumentErrors(t *testing.T) {
	if code := run(nil); code != 2 {
		t.Fatalf("no args code=%d want 2", code)
	}
	if code := run([]string{"frobnicate"}); code != 2 {
		t.Fatalf("unknown command code=%d want 2", code)
	}
	if code := run([]string{"demo"}); code != 2 {
		t.Fatalf("demo without root code=%d want 2", code)
	}
	if code := run([]string{"verify"}); code != 2 {
		t.Fatalf("verify without root code=%d want 2", code)
	}
	root := t.TempDir()
	if code := run([]string{"demo", "--root", root, "--fault-profile", "bogus"}); code != 2 {
		t.Fatalf("bad profile code=%d want 2", code)
	}
}

func TestScanFileStructuralExit3AndHealthyExit0(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.m4s")
	goodBytes := scan.BuildFragment(scan.FragmentSpec{
		TrackID: 1, BaseDecodeTime: 1, TFDTv1: true,
		Samples: []scan.SampleSpec{{Duration: 3000, Size: 4}},
	})
	if err := os.WriteFile(good, goodBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"scan-file", "--path", good}); code != 0 {
		t.Fatalf("healthy fragment code=%d want 0", code)
	}

	bad := scan.TruncateInsideTrun(goodBytes, 6)
	badPath := filepath.Join(dir, "bad.m4s")
	if err := os.WriteFile(badPath, bad, 0o644); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"scan-file", "--path", badPath}); code != 3 {
		t.Fatalf("truncated trun code=%d want 3", code)
	}
	if code := run([]string{"scan-file"}); code != 2 {
		t.Fatalf("scan-file without path code=%d want 2", code)
	}
}

func TestDemoVerifyExit0AndTamperExit4(t *testing.T) {
	root := t.TempDir()
	code := run([]string{"demo", "--root", root, "--seed", "41", "--segments", "12",
		"--fault-profile", "boundary-v1", "--resume-check"})
	if code != 0 {
		t.Fatalf("demo code=%d want 0", code)
	}
	if code := run([]string{"verify", "--root", root, "--repeat", "2"}); code != 0 {
		t.Fatalf("verify code=%d want 0", code)
	}

	// Tamper with a repaired output: index replay must disagree and the
	// verifier must fail with exit code 4.
	tampered := filepath.Join(root, "damaged", "out", "video", "seg_005.m4s")
	f, err := os.OpenFile(tampered, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte{0, 0, 0}); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if code := run([]string{"verify", "--root", root, "--repeat", "2"}); code != 4 {
		t.Fatalf("tampered verify code=%d want 4", code)
	}
}

func TestVerifyStructuralInputDamageExit3(t *testing.T) {
	root := t.TempDir()
	if code := run([]string{"demo", "--root", root, "--segments", "12"}); code != 0 {
		t.Fatalf("demo code=%d want 0", code)
	}
	// Replace an indexed INPUT fragment with a truncated-trun variant.
	src := filepath.Join(root, "damaged", "in", "video", "seg_004.m4s")
	good, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	bad := scan.TruncateInsideTrun(good, 6)
	if err := os.WriteFile(src, bad, 0o644); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"verify", "--root", root, "--repeat", "1"}); code != 3 {
		t.Fatalf("structural input verify code=%d want 3", code)
	}
}
