package main

import "testing"

func TestRunArgumentErrorsExit2(t *testing.T) {
	cases := [][]string{
		{"demo"},
		{"demo", "--root", "/tmp/whatever", "--cases", "bogus"},
		{"nope"},
	}
	for _, args := range cases {
		if rc := run(args); rc != exitArgs {
			t.Fatalf("args %v expected exit %d, got %d", args, exitArgs, rc)
		}
	}
}

func TestRunHelpExit0(t *testing.T) {
	if rc := run([]string{"help"}); rc != exitOK {
		t.Fatalf("help expected %d got %d", exitOK, rc)
	}
}

func TestRunDemoHappyPathExit0(t *testing.T) {
	rc := run([]string{"demo", "--seed", "41", "--root", t.TempDir(), "--cases", "clean"})
	if rc != exitOK {
		t.Fatalf("clean demo expected %d got %d", exitOK, rc)
	}
}

func TestRunDemoDamageReportsGapsStillPasses(t *testing.T) {
	// damage is allowed gaps; overall exit stays 0 because remaining gates pass.
	rc := run([]string{"demo", "--seed", "41", "--root", t.TempDir(), "--cases", "damage"})
	if rc != exitOK {
		t.Fatalf("damage demo expected %d got %d", exitOK, rc)
	}
}
