package main

import (
	"bytes"
	"testing"
	"time"
)

func TestUsageErrorsExitTwo(t *testing.T) {
	tests := [][]string{
		{},
		{"bogus"},
		{"selftest", "--unknown"},
		{"stats", "--unknown"},
		{"selftest", "positional"},
		{"selftest", "--duration", "bad"},
	}
	for _, args := range tests {
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code != 2 {
			t.Fatalf("args %v exit=%d, want 2", args, code)
		}
	}
}

func TestParseFaults(t *testing.T) {
	faults, err := parseFaults("drop=0.05,dup=0.01,reorder=0.2")
	if err != nil {
		t.Fatal(err)
	}
	if faults.Drop != 0.05 || faults.Duplicate != 0.01 || faults.Reorder != 0.2 {
		t.Fatalf("unexpected faults: %+v", faults)
	}
	if _, err := parseFaults("drop=2"); err == nil {
		t.Fatal("out-of-range fault should fail")
	}
}

func TestCommonFlagValidation(t *testing.T) {
	cfg := commonFlags{duration: time.Second, pps: 1, timeout: time.Second, seed: 1}
	if _, err := cfg.options(); err != nil {
		t.Fatal(err)
	}
	cfg.pps = 0
	if _, err := cfg.options(); err == nil {
		t.Fatal("zero pps should fail")
	}
}
