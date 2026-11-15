package plan

import "testing"

func TestSplitAndID(t *testing.T) {
	p := New("news", 25, 32, 32, 30, 10, 1200, 1)
	if len(p.Segments) != 3 || p.Segments[2].Frames != 5 {
		t.Fatalf("unexpected split: %#v", p.Segments)
	}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := p.EnsureID(); err != nil {
		t.Fatal(err)
	}
	if p.PlanID == "" || p.ComputeID() != p.PlanID {
		t.Fatal("plan ID is not stable")
	}
}

func TestZeroFramesIsValid(t *testing.T) {
	p := New("news", 0, 32, 32, 30, 10, 1200, 1)
	if err := p.Validate(); err != nil {
		t.Fatalf("zero-frame plan rejected: %v", err)
	}
}
