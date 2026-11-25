package clock

import "testing"

func TestUnwrapForwardWrap(t *testing.T) {
	u := NewUnwrapper(PCRModulus)
	const nearTop = PCRModulus - 1000
	if got, wrapped := u.Unwrap(nearTop); wrapped || got != int64(nearTop) {
		t.Fatalf("first sample got %d wrapped %v", got, wrapped)
	}
	got, wrapped := u.Unwrap(500)
	if !wrapped {
		t.Fatal("expected wrap")
	}
	if want := int64(PCRModulus) + 500; got != want {
		t.Fatalf("got %d want %d", got, want)
	}
	if u.Adjusts() != 1 {
		t.Fatalf("adjusts %d", u.Adjusts())
	}
}

func TestUnwrapMonotonicNoWrap(t *testing.T) {
	u := NewUnwrapper(PTSModulus)
	prev := int64(-1)
	for _, raw := range []uint64{10, 11, 5000, 1 << 20} {
		got, wrapped := u.Unwrap(raw)
		if wrapped {
			t.Fatalf("unexpected wrap at %d", raw)
		}
		if got <= prev {
			t.Fatalf("not monotonic: %d after %d", got, prev)
		}
		prev = got
	}
}

func TestUnwrapBackwardSeek(t *testing.T) {
	u := NewUnwrapper(PCRModulus)
	u.Unwrap(100)
	got, wrapped := u.Unwrap(PCRModulus - 100)
	if !wrapped || u.Adjusts() != -1 {
		t.Fatalf("expected backward adjust, got %v adjusts=%d", wrapped, u.Adjusts())
	}
	// re-anchoring at a negative absolute is legitimate for a backwards jump
	if got != -100 {
		t.Fatalf("got %d want -100", got)
	}
}

func TestUnwrapDoubleWrap(t *testing.T) {
	u := NewUnwrapper(PCRModulus)
	// Two samples exactly one modulus and 200 ticks apart: the raw second value
	// is numerically smaller yet the absolute delta is +200, so exactly one
	// boundary is crossed.
	base := uint64(PCRModulus - 300)
	first, _ := u.Unwrap(base)
	second, wrapped := u.Unwrap(200)
	if !wrapped {
		t.Fatal("expected wrap on net +200 after one modulus")
	}
	if delta := second - first; delta != 500 {
		t.Fatalf("delta %d want 500", delta)
	}
	if u.Adjusts() != 1 {
		t.Fatalf("adjusts %d", u.Adjusts())
	}
}

func TestMillisFormatting(t *testing.T) {
	cases := []struct {
		ticks int64
		want  string
	}{
		{0, "0.000"},
		{90000, "1000.000"},
		{9000, "100.000"},
		{9, "0.100"},
		{-9, "-0.100"},
		{NoTime, "n/a"},
	}
	for _, c := range cases {
		if got := FormatMS(c.ticks); got != c.want {
			t.Errorf("FormatMS(%d)=%q want %q", c.ticks, got, c.want)
		}
	}
}

func TestMillisConversions(t *testing.T) {
	if got := TicksForMillis(1000); got != Base {
		t.Fatalf("1000ms = %d ticks", got)
	}
	if got := Millis(Base); got != 1000 {
		t.Fatalf("90000 ticks = %f ms", got)
	}
	if got := TicksForSeconds(1); got != 90000 {
		t.Fatalf("1s = %d ticks", got)
	}
	if Rel(NoTime, 5) != NoTime || MaxRel(NoTime, 7) != 7 {
		t.Fatal("sentinel handling")
	}
}
