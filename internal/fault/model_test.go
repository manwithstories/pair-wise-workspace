package fault

import "testing"

func TestIntervalValidate(t *testing.T) {
	if err := (Interval{Start: 10, End: 5}).Validate(); err == nil {
		t.Fatal("inverted interval must fail")
	}
	if err := (Interval{Start: 5, End: 5}).Validate(); err != nil {
		t.Fatal("empty (zero-width) interval must be allowed")
	}
}

func TestMerge(t *testing.T) {
	in := []Interval{{0, 10}, {12, 20}, {21, 30}, {100, 110}}
	out, err := Merge(in, 2)
	if err != nil {
		t.Fatal(err)
	}
	want := []Interval{{0, 30}, {100, 110}}
	if len(out) != len(want) {
		t.Fatalf("merged %v, want %v", out, want)
	}
	for i := range want {
		if out[i] != want[i] {
			t.Fatalf("merged %v, want %v", out, want)
		}
	}
	if _, err := Merge(nil, -1); err == nil {
		t.Fatal("negative gap must fail")
	}
}

func TestOverlaps(t *testing.T) {
	if !(Interval{0, 10}).Overlaps(Interval{5, 15}) {
		t.Fatal("overlap not detected")
	}
	if (Interval{0, 10}).Overlaps(Interval{10, 20}) {
		t.Fatal("half-open boundary must not overlap")
	}
}

func TestFatalClassification(t *testing.T) {
	cases := map[Category]bool{
		CatBadChecksum:  true,
		CatTruncatedBox: true,
		CatIllegalSize:  true,
		CatDupTimestamp: true,
		CatRTPDuplicate: false,
		CatRTPLate:      false,
		CatBurstLoss:    false,
		CatGOPAnomaly:   false,
	}
	fs := make([]Fault, 0, len(cases))
	for cat, fatal := range cases {
		f := Fault{Cat: cat}
		if f.Fatal() != fatal {
			t.Fatalf("%s fatal=%v want %v", cat, f.Fatal(), fatal)
		}
		fs = append(fs, f)
	}
	if FatalCount(fs) != 4 {
		t.Fatalf("fatal count = %d, want 4", FatalCount(fs))
	}
}

func TestFaultErrorLocation(t *testing.T) {
	f := Fault{Cat: CatTruncatedBox, Byte: 4096, Ref: "mdat", Msg: "payload short"}
	if got := f.Error(); got == "" || got[0:1] != "t" {
		t.Fatalf("unexpected error text %q", got)
	}
}
