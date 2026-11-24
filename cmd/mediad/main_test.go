package main

import (
	"io"
	"path/filepath"
	"testing"

	"mediad/internal/fault"
	"mediad/internal/sample"
)

func TestCLIUsageExitCode(t *testing.T) {
	if code := Run(nil, io.Discard, io.Discard); code != fault.ExitUsage {
		t.Fatalf("no args code=%d want 2", code)
	}
	if code := Run([]string{"bogus"}, io.Discard, io.Discard); code != fault.ExitUsage {
		t.Fatalf("bogus code=%d want 2", code)
	}
}

func TestCLIInjectedExit75ThenResume(t *testing.T) {
	dir := t.TempDir()
	m, err := sample.Generate(sample.Options{
		Seed: 29, Seconds: 1, Rate: 48000, Channels: 2, Dir: dir, Faults: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	args := []string{
		"run", "--input", m.CapturePath, "--index", m.IndexPath,
		"--out", filepath.Join(dir, "final.mp4"), "--work", filepath.Join(dir, "work"),
		"--rate", "48000", "--channels", "2", "--packet-frames", "480",
	}
	injectCode := Run(append(append([]string{}, args...), "--inject-stage", "mp4"), io.Discard, io.Discard)
	if injectCode != fault.ExitInterrupted {
		t.Fatalf("inject code=%d want 75", injectCode)
	}
	resumeCode := Run(args, io.Discard, io.Discard)
	if resumeCode != fault.ExitOK {
		t.Fatalf("resume code=%d want 0", resumeCode)
	}
}
