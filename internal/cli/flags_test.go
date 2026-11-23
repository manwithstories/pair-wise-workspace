package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestDefaults(t *testing.T) {
	f, err := ParseDemo(nil, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if f.Seed != 41 || f.LossBurst != 7 || f.Duplicate != 3 || f.Reorder != 5 {
		t.Fatalf("defaults = %+v", f)
	}
	if f.BandwidthKbps != 1800 || f.TargetKbps != 2000 || f.CheckpointSamples != 120 {
		t.Fatalf("defaults = %+v", f)
	}
	if f.RotationSet {
		t.Fatal("rotation should be unset by default")
	}
}

func TestRotationSetTracking(t *testing.T) {
	f, err := ParseDemo([]string{"--rotation", "0"}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if !f.RotationSet || f.Rotation != 0 {
		t.Fatalf("explicit zero rotation not tracked: %+v", f)
	}
}

func TestValidateAcceptsContract(t *testing.T) {
	f, _ := ParseDemo([]string{
		"--seed", "41", "--loss-burst", "7", "--duplicate", "3", "--reorder", "5",
		"--bandwidth-kbps", "1800", "--target-kbps", "2000",
		"--checkpoint-samples", "120", "--resume",
	}, &bytes.Buffer{})
	if err := f.Validate(); err != nil {
		t.Fatal(err)
	}
	if !f.Resume {
		t.Fatal("resume flag not parsed")
	}
}

func TestValidateRejects(t *testing.T) {
	cases := [][]string{
		{"--checkpoint-samples", "0"},
		{"--checkpoint-samples", "-1"},
		{"--target-kbps", "0"},
		{"--bandwidth-kbps", "-1"},
		{"--loss-burst", "-2"},
		{"--duplicate", "-1"},
		{"--reorder", "-3"},
		{"--rotation", "45"},
		{"--rotation", "360"},
		{"--corrupt", "bogus"},
		{"--source", "avi"},
	}
	for _, args := range cases {
		f, err := ParseDemo(args, &bytes.Buffer{})
		if err != nil {
			continue // flag parse failure is also a rejection
		}
		if err := f.Validate(); err == nil {
			t.Fatalf("%v should be rejected", args)
		}
	}
}

func TestUnknownFlagIsError(t *testing.T) {
	var errb bytes.Buffer
	if _, err := ParseDemo([]string{"--nope"}, &errb); err == nil {
		t.Fatal("unknown flag should fail")
	}
	if !strings.Contains(errb.String(), "nope") {
		t.Fatalf("error output should name the flag: %q", errb.String())
	}
}
