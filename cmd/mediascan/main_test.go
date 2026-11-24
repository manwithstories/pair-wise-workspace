package main

import (
	"testing"
)

func TestExitCodes(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"no args", nil, 2},
		{"unknown command", []string{"frobnicate"}, 2},
		{"help", []string{"--help"}, 0},
		{"negative seed", []string{"demo", "--seed", "-5"}, 2},
		{"bad interrupt stage", []string{"demo", "--interrupt-after", "nope"}, 2},
		{"bad drop-every", []string{"demo", "--drop-every", "-1"}, 2},
		{"verify bad seed", []string{"verify", "--seed", "0"}, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := run(tc.args); got != tc.want {
				t.Fatalf("run(%v)=%d want %d", tc.args, got, tc.want)
			}
		})
	}
}

func TestDemoExitZero(t *testing.T) {
	dir := t.TempDir()
	if got := run([]string{"demo", "--seed", "41", "--drop-every", "97", "--data-dir", dir}); got != 0 {
		t.Fatalf("demo exit=%d", got)
	}
}

func TestDamagedDemoExitZero(t *testing.T) {
	dir := t.TempDir()
	if got := run([]string{"demo", "--seed", "41", "--damaged", "--data-dir", dir}); got != 0 {
		t.Fatalf("damaged demo exit=%d", got)
	}
}

func TestVerifyExitZero(t *testing.T) {
	dir := t.TempDir()
	if got := run([]string{"verify", "--seed", "41", "--data-dir", dir}); got != 0 {
		t.Fatalf("verify exit=%d", got)
	}
}

func TestInterruptWithoutResumeExitsOne(t *testing.T) {
	dir := t.TempDir()
	if got := run([]string{"demo", "--seed", "41", "--interrupt-after", "repair", "--data-dir", dir}); got != 1 {
		t.Fatalf("interrupted demo exit=%d, want 1", got)
	}
}
