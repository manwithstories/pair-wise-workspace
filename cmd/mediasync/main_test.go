package main

import (
	"testing"

	"mediasync/internal/demo"
)

func TestUsageErrorsExit2(t *testing.T) {
	cases := [][]string{
		{"bogus"},
		{"demo", "--fps", "24000/1001"},
		{"demo", "--fps", "30/0"},
		{"demo", "--faults", "flood"},
		{"demo", "--bitrates", "0"},
		{"demo", "--bitrates", ""},
		{"demo", "--crash-at", "ingest"},
	}
	for _, args := range cases {
		if code := run(args); code != demo.ExitUsage {
			t.Fatalf("run(%v) = %d, want 2", args, code)
		}
	}
}
