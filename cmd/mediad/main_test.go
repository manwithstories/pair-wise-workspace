package main

import "testing"

func TestRunArgumentAndBlockingExitCodes(t *testing.T) {
	if code := run(nil); code != 4 {
		t.Fatalf("no args code=%d want4", code)
	}
	if code := run([]string{"demo", "--unknown"}); code != 4 {
		t.Fatalf("unknown flag code=%d want4", code)
	}
	if code := run([]string{"demo", "--scenario", "bogus", "--out", t.TempDir()}); code != 4 {
		t.Fatalf("bogus scenario code=%d want4", code)
	}
	blocked := run([]string{"demo", "--scenario", "missing-init", "--seed", "73", "--out", t.TempDir()})
	if blocked != 3 {
		t.Fatalf("missing-init code=%d want3", blocked)
	}
}
