package main

import "testing"

func TestParseSelftestFlags(t *testing.T) {
	p, err := parseSelftestFlags([]string{
		"--out", "/tmp/out", "--seed", "7", "--duration", "8s",
		"--segment", "1s", "--bitrates", "450000,900000",
	})
	if err != nil {
		t.Fatalf("valid flags rejected: %v", err)
	}
	if p.Seed != 7 || p.DurationS != 8 || p.SegmentS != 1 {
		t.Errorf("scalar parse wrong: %+v", p)
	}
	if len(p.Bitrates) != 2 || p.Bitrates[0] != 450000 || p.Bitrates[1] != 900000 {
		t.Errorf("bitrates parse wrong: %+v", p.Bitrates)
	}
}

func TestParseSelftestFlagsDefaults(t *testing.T) {
	p, err := parseSelftestFlags([]string{"--out", "/tmp/out"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Seed != 41 || p.DurationS != 8 || p.SegmentS != 1 || len(p.Bitrates) != 2 {
		t.Errorf("defaults wrong: %+v", p)
	}
}

func TestParseSelftestFlagsErrors(t *testing.T) {
	bad := [][]string{
		{"--seed", "7"},               // missing out
		{"--out", "/tmp/o", "--duration", "1s"}, // too short
		{"--out", "/tmp/o", "--bitrates", "100"}, // single rung
		{"--out", "/tmp/o", "--seed", "-1"},
		{"--out", "/tmp/o", "--bogus", "1"},
		{"--out"},                     // value missing
		{"--out", "/tmp/o", "loose-token"},
	}
	for i, flags := range bad {
		if _, err := parseSelftestFlags(flags); err == nil {
			t.Errorf("case %d should have failed: %v", i, flags)
		}
	}
}

func TestParseSeconds(t *testing.T) {
	if n, err := parseSeconds("8s"); err != nil || n != 8 {
		t.Errorf("8s = %d, %v", n, err)
	}
	if n, err := parseSeconds("3"); err != nil || n != 3 {
		t.Errorf("3 = %d, %v", n, err)
	}
	if _, err := parseSeconds("0s"); err == nil {
		t.Error("zero seconds must fail")
	}
	if _, err := parseSeconds("abc"); err == nil {
		t.Error("non-numeric must fail")
	}
}

func TestParseBitrates(t *testing.T) {
	got, err := parseBitrates(" 100 , 200 ,300")
	if err != nil || len(got) != 3 || got[1] != 200 {
		t.Errorf("bitrate parse: %+v err=%v", got, err)
	}
	if _, err := parseBitrates("100,zero"); err == nil {
		t.Error("non-numeric bitrate must fail")
	}
	if _, err := parseBitrates("0"); err == nil {
		t.Error("zero bitrate must fail")
	}
}

func TestRunDispatch(t *testing.T) {
	if code := run(nil); code != exitBadArgument {
		t.Errorf("no args = %d, want %d", code, exitBadArgument)
	}
	if code := run([]string{"nope"}); code != exitBadArgument {
		t.Errorf("unknown cmd = %d, want %d", code, exitBadArgument)
	}
	if code := run([]string{"help"}); code != exitOK {
		t.Errorf("help = %d, want %d", code, exitOK)
	}
}
