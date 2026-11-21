package model

import (
	"errors"
	"math"
	"testing"
)

func TestFromStringExact(t *testing.T) {
	cases := []struct {
		in   string
		want Rational
	}{
		{"6", MustRational(6, 1)},
		{"6.0", MustRational(6, 1)},
		{"0.5", MustRational(1, 2)},
		{"1.999", MustRational(1999, 1000)},
		{"4/3", MustRational(4, 3)},
		{"0", Zero},
	}
	for _, c := range cases {
		got, err := FromString(c.in)
		if err != nil {
			t.Fatalf("FromString(%q): %v", c.in, err)
		}
		if !got.Equal(c.want) {
			t.Errorf("FromString(%q) = %s, want %s", c.in, got, c.want)
		}
	}
}

func TestFromStringRejectsGarbage(t *testing.T) {
	for _, in := range []string{"", "abc", "1/0", "1.2.3", "NaN"} {
		if _, err := FromString(in); err == nil {
			t.Errorf("FromString(%q) unexpectedly succeeded", in)
		}
	}
}

func TestFromTicksValidation(t *testing.T) {
	if _, err := FromTicks(90000, 0); !errors.Is(err, ErrInvalidTimebase) {
		t.Fatalf("zero timescale: %v", err)
	}
	if _, err := FromTicks(1, MaxTimescale+1); !errors.Is(err, ErrInvalidTimebase) {
		t.Fatalf("oversized timescale: %v", err)
	}
	if _, err := FromTicks(-1, 90000); !errors.Is(err, ErrNegativeTime) {
		t.Fatalf("negative ticks: %v", err)
	}
	got, err := FromTicks(90000, 90000)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(MustRational(1, 1)) {
		t.Fatalf("FromTicks(90000,90000)=%s", got)
	}
}

func TestAddSubCmp(t *testing.T) {
	a := MustRational(1, 3)
	b := MustRational(1, 6)
	sum, err := a.Add(b)
	if err != nil {
		t.Fatal(err)
	}
	if !sum.Equal(MustRational(1, 2)) {
		t.Fatalf("1/3+1/6=%s", sum)
	}
	diff, err := a.Sub(b)
	if err != nil {
		t.Fatal(err)
	}
	if !diff.Equal(MustRational(1, 6)) {
		t.Fatalf("1/3-1/6=%s", diff)
	}
	if a.Cmp(b) != 1 || b.Cmp(a) != -1 || a.Cmp(a) != 0 {
		t.Fatal("Cmp ordering wrong")
	}
}

func TestOverflowDetected(t *testing.T) {
	_, err := NewRational(1, math.MinInt64)
	if !errors.Is(err, ErrTimeOverflow) {
		t.Fatalf("want overflow, got %v", err)
	}
	big := MustRational(math.MaxInt64/2, 1)
	if _, err := big.MulInt(4); !errors.Is(err, ErrTimeOverflow) {
		t.Fatalf("mul overflow: %v", err)
	}
	x := MustRational(math.MaxInt64, 3)
	y := MustRational(1, math.MaxInt64)
	if _, err := x.Add(y); !errors.Is(err, ErrTimeOverflow) {
		t.Fatalf("add overflow should fail, got %v", err)
	}
}

func TestCmpWideFallback(t *testing.T) {
	if Zero.Cmp(Zero) != 0 {
		t.Fatal("zero comparison broken")
	}
	// Denominators large enough that Num1*Den2 overflows int64.
	a := MustRational(math.MaxInt64/2, math.MaxInt64/3)
	b := MustRational(math.MaxInt64/3, math.MaxInt64/2)
	if a.Cmp(b) != 1 {
		t.Fatalf("wide compare wrong: %d", a.Cmp(b))
	}
}

func TestMillisRounded(t *testing.T) {
	cases := []struct {
		r    Rational
		want int64
	}{
		{MustRational(1, 3), 333},
		{MustRational(2, 3), 667},
		{MustRational(1, 2), 500},
		{MustRational(40, 1000), 40},
		{MustRational(-1, 3), -333},
		{MustRational(1500, 1000), 1500},
		{MustRational(1, 2000), 1},
	}
	for _, c := range cases {
		got, err := c.r.MillisRounded()
		if err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Errorf("%s -> %dms, want %d", c.r, got, c.want)
		}
	}
}

func TestFaultsOrderingAndCounts(t *testing.T) {
	var fs Faults
	fs.Add(Fault{Kind: FaultAVSync, Severity: SeverityWarning, TimeStart: MustRational(2, 1), IndexEntry: 2})
	fs.Add(Fault{Kind: FaultMissingSegment, Severity: SeverityCritical, TimeStart: MustRational(1, 1), IndexEntry: 1})
	fs.Add(Fault{Kind: FaultMissingSegment, Severity: SeverityCritical, TimeStart: MustRational(3, 1), IndexEntry: 3})
	fs.Sort()
	all := fs.All()
	if all[0].TimeStart.Cmp(MustRational(1, 1)) != 0 {
		t.Fatalf("sort failed: %v", all)
	}
	first, err := fs.First(FaultMissingSegment)
	if err != nil {
		t.Fatal(err)
	}
	if first.IndexEntry != 1 {
		t.Fatalf("first missing should be entry 1, got %d", first.IndexEntry)
	}
	counts := fs.Counts()
	if counts[FaultMissingSegment] != 2 || counts[FaultAVSync] != 1 {
		t.Fatalf("counts: %v", counts)
	}
	if _, err := fs.First(FaultPacketLoss); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want not found, got %v", err)
	}
}

func TestFaultValidate(t *testing.T) {
	good := Fault{Kind: FaultTruncatedBox, Severity: SeverityCritical,
		ByteStart: 10, ByteEnd: 20, TimeStart: Zero, TimeEnd: MustRational(1, 1)}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := good
	bad.ByteEnd = 5
	if err := bad.Validate(); err == nil {
		t.Fatal("inverted byte range accepted")
	}
	bad = good
	bad.Kind = "nope"
	if err := bad.Validate(); err == nil {
		t.Fatal("unknown kind accepted")
	}
}
