package main

import "testing"

func TestDispatchUsage(t *testing.T) {
	if code := run(nil); code != 2 {
		t.Fatalf("no args: code=%d", code)
	}
	if code := run([]string{"bogus"}); code != 2 {
		t.Fatalf("unknown subcommand: code=%d", code)
	}
	if code := run([]string{"seek"}); code != 2 {
		t.Fatalf("missing params: code=%d", code)
	}
}
