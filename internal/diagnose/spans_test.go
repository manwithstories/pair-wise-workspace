package diagnose

import (
	"testing"

	"fieldqc/internal/clock"
	"fieldqc/internal/fault"
)

func sec(n int64) clock.Ratio { return clock.Ratio{Num: n, Den: 1} }

func sp(track string, code fault.Code, a, b int64) fault.Span {
	return fault.Span{Track: track, Code: code, Start: sec(a), End: sec(b)}
}

func TestMergeEmpty(t *testing.T) {
	if got := Merge(nil); got != nil {
		t.Fatalf("Merge(nil)=%v, want nil", got)
	}
}

func TestMergeOverlappingAndAdjacentSameTrackCode(t *testing.T) {
	// Deliberately unsorted; [0,10)+[5,15) overlap -> [0,15);
	// [15,20) is adjacent -> extends to [0,20).
	in := []fault.Span{
		sp("v", fault.CodeTruncatedBox, 5, 15),
		sp("v", fault.CodeTruncatedBox, 0, 10),
		sp("v", fault.CodeTruncatedBox, 15, 20),
	}
	got := Merge(in)
	if len(got) != 1 {
		t.Fatalf("len=%d, want 1 merged span: %+v", len(got), got)
	}
	if got[0].Start.Cmp(sec(0)) != 0 || got[0].End.Cmp(sec(20)) != 0 {
		t.Fatalf("merged range = %s..%s, want 0..20", got[0].Start, got[0].End)
	}
}

func TestMergeKeepsGapSeparate(t *testing.T) {
	in := []fault.Span{
		sp("v", fault.CodeTruncatedBox, 0, 5),
		sp("v", fault.CodeTruncatedBox, 8, 12),
	}
	if got := Merge(in); len(got) != 2 {
		t.Fatalf("len=%d, want 2 across a gap: %+v", len(got), got)
	}
}

func TestMergeDistinguishesCodeAndTrack(t *testing.T) {
	in := []fault.Span{
		sp("v", fault.CodeTruncatedBox, 0, 10),
		sp("v", fault.CodeTimestampJump, 0, 10), // same range, diff code
		sp("a", fault.CodeTruncatedBox, 0, 10),  // diff track
	}
	got := Merge(in)
	if len(got) != 3 {
		t.Fatalf("len=%d, want 3 (code/track prevent merge): %+v", len(got), got)
	}
	// Output must be deterministically sorted by track then start/code.
	if got[0].Track != "a" || got[1].Code != fault.CodeTimestampJump || got[2].Code != fault.CodeTruncatedBox {
		t.Fatalf("merged order unexpected: %+v", got)
	}
}

func TestMergeDoesNotMutateInputOrder(t *testing.T) {
	in := []fault.Span{
		sp("v", fault.CodeTruncatedBox, 9, 10),
		sp("v", fault.CodeTruncatedBox, 0, 1),
	}
	_ = Merge(in)
	if in[0].Start.Cmp(sec(9)) != 0 {
		t.Fatal("Merge mutated the caller's slice order")
	}
}
