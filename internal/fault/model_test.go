package fault

import (
	"errors"
	"fmt"
	"testing"
)

func TestErrorRendersLocator(t *testing.T) {
	e := New(CodeCorruptContainer, Location{SourcePath: "clip.mov", AssetID: "A1"}.
		WithTrack(2).WithOffset(4096), "truncated moov").
		WithOp("mp4.ParseTrak").
		WithRecovery("asset isolated, continue")
	s := e.Error()
	for _, want := range []string{
		"error_code=corrupt_container", "asset=A1", "path=clip.mov",
		"track=2", "offset=4096", "op=mp4.ParseTrak", "recovery:",
	} {
		if !contains(s, want) {
			t.Fatalf("rendered error missing %q: %s", want, s)
		}
	}
}

func TestCodeMatchingThroughCause(t *testing.T) {
	inner := New(CodeBadIndex, Location{}.WithOffset(12), "stss out of range")
	wrapped := fmt.Errorf("context: %w", inner)
	if CodeOf(wrapped) != CodeBadIndex {
		t.Fatalf("CodeOf = %v", CodeOf(wrapped))
	}
	if !errors.Is(wrapped, New(CodeBadIndex, Location{}, "")) {
		t.Fatal("errors.Is should match by code")
	}
}

func TestExitCodes(t *testing.T) {
	cases := map[Code]int{
		CodeCorruptContainer: 2,
		CodeMapping:          2,
		CodeVerify:           3,
		CodeDigestMismatch:   3,
		CodeCheckpoint:       4,
		CodeAdapterChanged:   4,
		CodeUnknown:          1,
	}
	for code, want := range cases {
		if got := ExitCode(New(code, Location{}, "x")); got != want {
			t.Fatalf("%s: ExitCode=%d want %d", code, got, want)
		}
	}
	if ExitCode(nil) != 0 {
		t.Fatal("nil error should exit 0")
	}
}

func TestAggregateKeepsGoing(t *testing.T) {
	var a Aggregate
	if a.Add(NewRecoverable(CodeNoPackets, Location{}.WithTrack(3), "empty track")) {
		t.Fatal("recoverable must not signal fatal")
	}
	if !a.Add(New(CodeCorruptContainer, Location{}, "boom")) {
		t.Fatal("fatal must signal")
	}
	if a.Recovered() != 1 || !a.Fatal() {
		t.Fatalf("aggregate counts wrong: recovered=%d fatal=%v", a.Recovered(), a.Fatal())
	}
	if CodeOf(a.Errors[0]) != CodeNoPackets {
		t.Fatal("first code lost")
	}
	if len(a.Unwrap()) != 2 {
		t.Fatal("unwrap length")
	}
}

func TestAllCodesUnique(t *testing.T) {
	seen := map[Code]bool{}
	for _, c := range AllCodes() {
		if seen[c] {
			t.Fatalf("duplicate code %q", c)
		}
		seen[c] = true
	}
}

func contains(hay, needle string) bool {
	return len(hay) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(hay); i++ {
			if hay[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
