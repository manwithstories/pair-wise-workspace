package clock

import (
	"encoding/json"
	"testing"
)

func TestTickConversionIsExact(t *testing.T) {
	// 1001 ticks at 1000 ticks/s is exactly 1.001s.
	r, err := FromTicks(1001, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.String(); got != "1.001" {
		t.Fatalf("String()=%q want 1.001", got)
	}
	oneMS := MustNew(1, 1000)
	if !WithinTolerance(r, MustNew(1, 1), oneMS) {
		t.Fatal("1ms offset must sit exactly on the tolerance boundary")
	}
	later, _ := FromTicks(1002, 1000)
	if WithinTolerance(later, MustNew(1, 1), oneMS) {
		t.Fatal("2ms offset must exceed the 1ms tolerance")
	}
}

func TestParseDecimalAndScale(t *testing.T) {
	scale, err := ParseDecimal("1.001")
	if err != nil {
		t.Fatal(err)
	}
	ticks, _ := FromTicks(48, 1000)
	got := ticks.Mul(scale)
	if got.Cmp(MustNew(48048, 1000000)) != 0 {
		t.Fatalf("drift scale mismatch: %s", got)
	}
	if millis := got.Millis(); millis < 48.047 || millis > 48.049 {
		t.Fatalf("Millis()=%v", millis)
	}
}

func TestArithmeticAndOrdering(t *testing.T) {
	a := MustNew(1, 3)
	b := MustNew(1, 6)
	if a.Add(b).Cmp(MustNew(1, 2)) != 0 {
		t.Fatalf("add: %s", a.Add(b))
	}
	if a.Sub(b).Cmp(b) != 0 {
		t.Fatalf("sub: %s", a.Sub(b))
	}
	if a.Quo(b).Cmp(MustNew(2, 1)) != 0 {
		t.Fatalf("quo: %s", a.Quo(b))
	}
	if a.Neg().Sign() >= 0 || !a.Neg().IsNegative() {
		t.Fatal("neg sign")
	}
	if Min(a, b) != b || Max(a, b) != a {
		t.Fatal("min/max")
	}
	if !a.Equal(MustNew(2, 6)) || a.Cmp(MustNew(1, 4)) <= 0 {
		t.Fatal("equality/order")
	}
	if !(Rational{}).IsZero() {
		t.Fatal("zero value must be zero")
	}
	var zero Rational
	if zero.Add(a).Cmp(a) != 0 {
		t.Fatal("zero value addition")
	}
}

func TestRejectsBadInput(t *testing.T) {
	if _, err := New(1, 0); err == nil {
		t.Fatal("zero denominator accepted")
	}
	if _, err := New(1, -2); err == nil {
		t.Fatal("negative denominator accepted")
	}
	if _, err := FromTicks(1, 0); err == nil {
		t.Fatal("zero timescale accepted")
	}
	if _, err := ParseDecimal("nope"); err == nil {
		t.Fatal("bad decimal accepted")
	}
}

func TestJSONRoundTrip(t *testing.T) {
	in := MustNew(1001, 1000)
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out Rational
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if !in.Equal(out) {
		t.Fatalf("round trip %s != %s", in, out)
	}
}
