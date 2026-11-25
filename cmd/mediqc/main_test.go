package main

import "testing"

func TestCLIRunAndVerify(t *testing.T) {
	dir := t.TempDir()
	if code := run([]string{"run", "--seed", "412", "--suite", "all", "--out", dir}); code != 0 {
		t.Fatalf("run exit code = %d", code)
	}
	if code := run([]string{"verify", "--dir", dir}); code != 0 {
		t.Fatalf("verify exit code = %d", code)
	}
}

func TestCLIUnsupportedReturnsTwo(t *testing.T) {
	if code := run([]string{"run", "--suite", "mxf", "--out", t.TempDir()}); code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
}
