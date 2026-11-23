package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDemoInterruptThenResume(t *testing.T) {
	work := filepath.Join(t.TempDir(), "archive")
	if code := run([]string{"demo", "--work", work, "--queue", "4", "--slow-consumer", "1ms", "--interrupt-after", "7"}); code != exitCancelled {
		t.Fatalf("interrupt exit = %d, want %d", code, exitCancelled)
	}
	if _, err := os.Stat(filepath.Join(work, "checkpoint.json")); err != nil {
		t.Fatalf("interrupt did not persist a checkpoint: %v", err)
	}
	if code := run([]string{"demo", "--work", work, "--queue", "4", "--resume"}); code != exitOK {
		t.Fatalf("resume exit = %d, want %d", code, exitOK)
	}
	if code := run([]string{"verify", "--work", work}); code != exitOK {
		t.Fatalf("verify exit = %d", code)
	}
}

func TestDemoScriptedInterruptResume(t *testing.T) {
	work := filepath.Join(t.TempDir(), "archive")
	if code := run([]string{"demo", "--work", work, "--queue", "4", "--slow-consumer", "1ms", "--interrupt-after", "7", "--resume"}); code != exitOK {
		t.Fatalf("scripted demo exit = %d", code)
	}
	if code := run([]string{"verify", "--work", work}); code != exitOK {
		t.Fatalf("verify exit = %d", code)
	}
}

func TestResumeRejectsIncompatibleInput(t *testing.T) {
	work := filepath.Join(t.TempDir(), "archive")
	if code := run([]string{"demo", "--work", work, "--interrupt-after", "5"}); code != exitCancelled {
		t.Fatalf("interrupt exit = %d", code)
	}
	// Perturb the input corpus so the manifest digest changes.
	manifest := filepath.Join(work, "in", "manifest.txt")
	data, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(string(data), "media frag-013.m4s\n", "", 1)
	if err := os.WriteFile(manifest, []byte(changed), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"demo", "--work", work, "--resume"}); code != exitIncompatible {
		t.Fatalf("incompatible resume exit = %d, want %d", code, exitIncompatible)
	}
}

func TestVerifyDetectsTamperedOutput(t *testing.T) {
	work := filepath.Join(t.TempDir(), "archive")
	if code := run([]string{"demo", "--work", work, "--queue", "4", "--slow-consumer", "0s"}); code != exitOK {
		t.Fatalf("demo exit = %d", code)
	}
	out := filepath.Join(work, "output.acx")
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)/2] ^= 0xff
	if err := os.WriteFile(out, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"verify", "--work", work}); code != exitFailure {
		t.Fatalf("tampered verify exit = %d, want %d", code, exitFailure)
	}
}

func TestVerifyRepeatableSHA(t *testing.T) {
	work1 := filepath.Join(t.TempDir(), "a")
	work2 := filepath.Join(t.TempDir(), "b")
	for _, w := range []string{work1, work2} {
		if code := run([]string{"demo", "--work", w, "--queue", "2"}); code != exitOK {
			t.Fatalf("demo %s exit = %d", w, code)
		}
	}
	a, _ := os.ReadFile(filepath.Join(work1, "output.acx"))
	b, _ := os.ReadFile(filepath.Join(work2, "output.acx"))
	if string(a) != string(b) {
		t.Fatal("repeated runs produced different outputs")
	}
}
