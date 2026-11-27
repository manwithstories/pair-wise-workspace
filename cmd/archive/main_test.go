package main

import (
	"testing"
)

func TestParseBandwidth(t *testing.T) {
	got, err := parseBandwidth("900,4200,1200")
	if err != nil || len(got) != 3 || got[1] != 4200 {
		t.Fatalf("parseBandwidth = %v, %v", got, err)
	}
	if _, err := parseBandwidth("100,0,-5"); err == nil {
		t.Fatal("non-positive bandwidth must be rejected")
	}
	if _, err := parseBandwidth("abc"); err == nil {
		t.Fatal("non-numeric bandwidth must be rejected")
	}
}

func TestParseFormats(t *testing.T) {
	cases := map[string]int{"hls": 1, "dash": 1, "both": 2}
	for in, want := range cases {
		got, err := parseFormats(in)
		if err != nil || len(got) != want {
			t.Fatalf("parseFormats(%q) = %v, %v", in, got, err)
		}
	}
	if _, err := parseFormats("mp4"); err == nil {
		t.Fatal("unknown format must be rejected")
	}
}

func TestValidateFlags(t *testing.T) {
	good := demoFlags{seed: 41, targetKbps: 1800, memoryBytes: 8388608}
	if err := validateFlags(good); err != nil {
		t.Fatalf("valid flags rejected: %v", err)
	}
	bad := good
	bad.targetKbps = 0
	if err := validateFlags(bad); err == nil {
		t.Fatal("zero target must fail")
	}
	bad = good
	bad.memoryBytes = 1 << 40
	if err := validateFlags(bad); err == nil {
		t.Fatal("oversize memory must fail")
	}
	bad = good
	bad.reorderWindow = 999999
	if err := validateFlags(bad); err == nil {
		t.Fatal("oversize reorder window must fail")
	}
}

func TestRunExitCodes(t *testing.T) {
	// Unknown command -> 1.
	if code := run([]string{"frobnicate"}); code != 1 {
		t.Fatalf("unknown command exit = %d, want 1", code)
	}
	// Help -> 0.
	if code := run([]string{"help"}); code != 0 {
		t.Fatalf("help exit = %d, want 0", code)
	}
	// Bad demo parameter -> 1.
	if code := run([]string{"demo", "--target-kbps", "0"}); code != 1 {
		t.Fatalf("bad param exit = %d, want 1", code)
	}
}

func TestRunDemoHappyPath(t *testing.T) {
	code := run([]string{"demo", "--format", "hls", "--seed", "41",
		"--target-kbps", "1800", "--bandwidth-kbps", "900,4200,1200",
		"--duplicate-every", "7", "--reorder-window", "9",
		"--loss-burst", "4", "--memory-bytes", "8388608"})
	if code != 0 {
		t.Fatalf("happy path exit = %d, want 0", code)
	}
}

func TestRunDemoStrictExitTwo(t *testing.T) {
	code := run([]string{"demo", "--format", "hls", "--seed", "41",
		"--loss-burst", "4", "--strict", "--memory-bytes", "8388608"})
	if code != 2 {
		t.Fatalf("strict damaged exit = %d, want 2", code)
	}
}
