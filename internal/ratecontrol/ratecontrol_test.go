package ratecontrol

import (
	"math"
	"testing"
)

func TestVBVFeedbackDirections(t *testing.T) {
	low := NewVBV(1000)
	low.Observe(5000)
	if low.QMultiplier() <= 1 {
		t.Fatalf("empty buffer should tighten q, got %.3f", low.QMultiplier())
	}
	high := NewVBV(1000)
	high.Observe(0)
	if high.QMultiplier() >= 1 {
		t.Fatalf("full buffer should relax q, got %.3f", high.QMultiplier())
	}
}

func TestAllocatePreservesBudgetAndWeights(t *testing.T) {
	demands := []SegmentDemand{
		{Frames: 10, Complexity: 800},
		{Frames: 10, Complexity: 1600},
		{Frames: 10, Complexity: 800, HardCut: true},
	}
	targets := Allocate(1200, demands)
	if targets[1] <= targets[0] || targets[2] <= targets[0] {
		t.Fatalf("complexity/hard-cut weights not applied: %v", targets)
	}
	mean := (targets[0] + targets[1] + targets[2]) / 3
	if math.Abs(mean-1200) > 1e-9 {
		t.Fatalf("weighted mean %.6f, want 1200", mean)
	}
}
