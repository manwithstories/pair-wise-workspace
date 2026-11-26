package main

import (
	"testing"

	"mediamix/internal/clock"
)

func TestParseArgsDefaultsAndValid(t *testing.T) {
	o, err := parseArgs([]string{"demo"})
	if err != nil {
		t.Fatal(err)
	}
	if o.caseName != "all" || o.seed != 41 || o.queueMiB != 8 || o.seconds != 12 {
		t.Fatalf("unexpected defaults: %+v", o)
	}
	o2, err := parseArgs([]string{
		"demo", "--case", "normal", "--seed", "7", "--seconds", "3",
		"--loss", "0.1", "--reorder", "0.2", "--queue-mib", "4",
		"--slow-consumer-ms", "5", "--fault", "before-commit",
	})
	if err != nil {
		t.Fatal(err)
	}
	if o2.caseName != "normal" || o2.seed != 7 || o2.queueMiB != 4 || o2.slowConsumerMs != 5 {
		t.Fatalf("parsed values wrong: %+v", o2)
	}
}

func TestParseArgsRejectsBadInput(t *testing.T) {
	bad := [][]string{
		{"demo", "--queue-mib", "99"},
		{"demo", "--queue-mib", "0"},
		{"demo", "--seconds", "0"},
		{"demo", "--seconds", "601"},
		{"demo", "--loss", "1.5"},
		{"demo", "--reorder", "-0.1"},
		{"demo", "--case", "bogus"},
		{"demo", "--fault", "nope"},
		{"demo", "--seed"},
		{"demo", "--unknown", "1"},
		{"notdemo"},
	}
	for i, args := range bad {
		if _, err := parseArgs(args); err == nil {
			t.Fatalf("case %d %v should be rejected", i, args)
		} else if _, ok := err.(*inputError); !ok {
			t.Fatalf("case %d: expected inputError, got %T", i, err)
		}
	}
}

func TestMediaErrorWrapping(t *testing.T) {
	me := asMediaError(&inputError{msg: "x"})
	if !clock.IsCode(me, clock.ErrInvalidParam) {
		t.Fatalf("expected invalid_param code, got %v", me)
	}
}

func TestSelectCases(t *testing.T) {
	if got := selectCases("all"); len(got) != 3 || got[0] != "normal" {
		t.Fatalf("all cases wrong: %v", got)
	}
	if got := selectCases("corrupt"); len(got) != 1 || got[0] != "corrupt" {
		t.Fatalf("single case wrong: %v", got)
	}
}
