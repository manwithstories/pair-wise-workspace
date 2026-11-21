package main

import "testing"

func TestRunExitCodes(t *testing.T) {
	if code := run([]string{"mediadeliver"}); code != 2 {
		t.Fatalf("no args: %d", code)
	}
	if code := run([]string{"mediadeliver", "demo", "--channels", "0"}); code != 2 {
		t.Fatalf("bad param: %d", code)
	}
	code := run([]string{"mediadeliver", "demo", "--seed", "42",
		"--sample-rate", "48000", "--channels", "2",
		"--target-lufs", "-16", "--rtp-window", "32"})
	if code != 0 {
		t.Fatalf("demo: %d", code)
	}
}
