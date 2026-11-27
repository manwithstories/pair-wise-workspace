package main

import (
	"os"
	"path/filepath"
	"testing"

	"avpipe/internal/media"
	"avpipe/internal/wav"
)

func TestRunUsageExitCodes(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"no args", nil, int(media.ExitUsage)},
		{"unknown", []string{"bogus"}, int(media.ExitUsage)},
		{"help", []string{"help"}, int(media.ExitOK)},
		{"version", []string{"version"}, int(media.ExitOK)},
		{"demo bad flag", []string{"demo", "--nope"}, int(media.ExitUsage)},
		{"demo extra arg", []string{"demo", "--out", "/tmp/x", "stray"}, int(media.ExitUsage)},
		{"check no path", []string{"check"}, int(media.ExitUsage)},
		{"check bad block", []string{"check", "--block", "0", "x.wav"}, int(media.ExitUsage)},
		{"check missing file", []string{"check", filepath.Join(t.TempDir(), "absent.wav")}, int(media.ExitInternal)},
	}
	for _, tc := range cases {
		if got := run(tc.args); got != tc.want {
			t.Fatalf("%s: run(%v)=%d want %d", tc.name, tc.args, got, tc.want)
		}
	}
}

func TestCheckExitCodes(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.wav")
	f, err := os.Create(good)
	if err != nil {
		t.Fatal(err)
	}
	rate := media.Rate{Num: 48000, Den: 1}
	samples := make([]float64, 800)
	if err := wav.WritePCM16(f, rate, 1, samples); err != nil {
		t.Fatal(err)
	}
	f.Close()

	if got := run([]string{"check", good}); got != int(media.ExitOK) {
		t.Fatalf("good file exit=%d", got)
	}
	bad := filepath.Join(dir, "bad.wav")
	data, err := os.ReadFile(good)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, data[:len(data)-3], 0o644); err != nil {
		t.Fatal(err)
	}
	if got := run([]string{"check", bad}); got != int(media.ExitInput) {
		t.Fatalf("truncated file exit=%d want %d", got, media.ExitInput)
	}
	if got := run([]string{"check", good, good}); got != int(media.ExitUsage) {
		t.Fatalf("two paths exit=%d", got)
	}
}
