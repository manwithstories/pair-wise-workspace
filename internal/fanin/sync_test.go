package fanin

import "testing"

func TestCheckAligned(t *testing.T) {
	perRung := map[string][]Boundary{
		"r0": {{Rung: "r0", Seq: 0, Time: 0}, {Rung: "r0", Seq: 25, Time: 90000}},
		"r1": {{Rung: "r1", Seq: 0, Time: 0}, {Rung: "r1", Seq: 25, Time: 90000}},
		"r2": {{Rung: "r2", Seq: 0, Time: 0}, {Rung: "r2", Seq: 25, Time: 90000}},
	}
	if m := Check(perRung, 0); len(m) != 0 {
		t.Fatalf("mismatches=%v", m)
	}
}

func TestCheckMissingKeyframe(t *testing.T) {
	perRung := map[string][]Boundary{
		"r0": {{Rung: "r0", Seq: 0, Time: 0}, {Rung: "r0", Seq: 25, Time: 90000}},
		"r1": {{Rung: "r1", Seq: 0, Time: 0}},
	}
	m := Check(perRung, 0)
	if len(m) != 1 || m[0].Seq != 25 {
		t.Fatalf("mismatches=%v", m)
	}
}

func TestCheckTimeSkew(t *testing.T) {
	perRung := map[string][]Boundary{
		"r0": {{Rung: "r0", Seq: 0, Time: 0}},
		"r1": {{Rung: "r1", Seq: 0, Time: 5000}},
	}
	if m := Check(perRung, 1000); len(m) != 1 {
		t.Fatalf("want 1 mismatch, got %v", m)
	}
	if m := Check(perRung, 6000); len(m) != 0 {
		t.Fatalf("within tolerance, got %v", m)
	}
}
