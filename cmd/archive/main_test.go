package main

import (
	"testing"

	"github.com/archive/rtprestore/internal/checkpoint"
	"github.com/archive/rtprestore/internal/verify"
)

func TestParseDemo(t *testing.T) {
	d, err := parseDemo([]string{"--seed", "41", "--loss", "3", "--out", "/tmp/x"})
	if err != nil {
		t.Fatal(err)
	}
	if d.seed != 41 || d.loss != 3 || d.out != "/tmp/x" {
		t.Fatalf("parsed: %+v", d)
	}
	if _, err := parseDemo([]string{"--seed", "1"}); err == nil {
		t.Fatal("missing --out must error")
	}
	if _, err := parseDemo([]string{"--checkpoint-every", "0", "--out", "/tmp/x"}); err == nil {
		t.Fatal("non-positive checkpoint interval must error")
	}
}

func TestParseVerify(t *testing.T) {
	v, err := parseVerify([]string{"--out", "/tmp/x", "--inject", "torn-tail"})
	if err != nil {
		t.Fatal(err)
	}
	if v.inject != "torn-tail" {
		t.Fatalf("inject: %q", v.inject)
	}
	if _, err := parseVerify(nil); err == nil {
		t.Fatal("missing --out must error")
	}
	if _, err := parseVerify([]string{"--out", "/tmp/x", "--inject", "bogus"}); err == nil {
		t.Fatal("unsupported inject value must error")
	}
}

func TestMapErrorCodes(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"unreadable", verify.ErrUnreadable, exitBadArgs},
		{"params", checkpoint.ErrParamsChanged, exitUnsafeCheckpoint},
		{"corrupt", checkpoint.ErrCheckpointCorrupt, exitUnsafeCheckpoint},
	}
	for _, c := range cases {
		if got := mapError(c.err); got != c.want {
			t.Fatalf("%s: got %d want %d", c.name, got, c.want)
		}
	}
}
