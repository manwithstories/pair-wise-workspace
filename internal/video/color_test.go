package video

import (
	"testing"

	"mediatool/internal/clock"
)

func TestResolveAgreement(t *testing.T) {
	m := Metadata{Primaries: "bt709", Transfer: "bt709", Matrix: "bt709", Range: "limited"}
	rec, fault, err := Resolve([]Input{
		{Source: "cam-a", Priority: 0, Meta: m},
		{Source: "cam-b", Priority: 1, Meta: m},
	})
	if err != nil || fault != nil {
		t.Fatalf("fault=%v err=%v", fault, err)
	}
	if rec.Canonical != m || rec.Reason != "all inputs agree" {
		t.Fatalf("rec = %+v", rec)
	}
}

func TestResolveConflictByPriority(t *testing.T) {
	m709 := Metadata{Primaries: "bt709", Transfer: "bt709", Matrix: "bt709", Range: "limited"}
	m2020 := Metadata{Primaries: "bt2020", Transfer: "pq", Matrix: "bt2020nc", Range: "full"}
	rec, fault, err := Resolve([]Input{
		{Source: "cam-b", Priority: 5, Meta: m2020},
		{Source: "cam-a", Priority: 1, Meta: m709},
	})
	if err != nil {
		t.Fatal(err)
	}
	if fault == nil || fault.Kind != clock.FaultColorConflict || !fault.Recovered {
		t.Fatalf("fault = %v", fault)
	}
	if rec.Canonical != m709 {
		t.Fatalf("canonical = %+v, want bt709 set", rec.Canonical)
	}
	if len(rec.Inputs) != 2 {
		t.Fatalf("original inputs not retained: %+v", rec.Inputs)
	}
	if rec.Reason == "" {
		t.Fatal("reason must be recorded")
	}
}

func TestResolveEmpty(t *testing.T) {
	if _, _, err := Resolve(nil); err == nil {
		t.Fatal("want error for empty inputs")
	}
}
