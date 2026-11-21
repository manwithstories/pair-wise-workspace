package timeline

import (
	"strings"
	"testing"

	"mediacheck/internal/fault"
)

func mustRate(t *testing.T, s string) Rate {
	t.Helper()
	r, err := ParseRate(s)
	if err != nil {
		t.Fatalf("ParseRate(%q): %v", s, err)
	}
	return r
}

func TestParseRate(t *testing.T) {
	for _, s := range []string{"24", "25", "30", "30000/1001"} {
		if _, err := ParseRate(s); err != nil {
			t.Errorf("ParseRate(%q) unexpected error: %v", s, err)
		}
	}
	if _, err := ParseRate("abc"); err == nil {
		t.Error("ParseRate(abc) should fail")
	}
	if _, err := ParseRate("30/0"); err == nil {
		t.Error("ParseRate(30/0) should fail")
	}
}

func TestTimecodeNonDrop(t *testing.T) {
	r25 := mustRate(t, "25")
	tc, err := ParseTimecode("01:00:00:00", r25, 0)
	if err != nil {
		t.Fatal(err)
	}
	if tc.Frame != 90000 || tc.Nanos != 3600*NanosPerSecond {
		t.Errorf("got frame=%d nanos=%d", tc.Frame, tc.Nanos)
	}
	// Round-trip at every supported rate.
	for _, rs := range []string{"24", "25", "30", "30000/1001"} {
		r := mustRate(t, rs)
		tc, err := ParseTimecode("02:03:04:05", r, 0)
		if err != nil {
			t.Fatalf("%s: %v", rs, err)
		}
		if got := Format(tc.Nanos, r, false); got != "02:03:04:05" {
			t.Errorf("%s round-trip got %q", rs, got)
		}
	}
}

func TestDropFrame(t *testing.T) {
	r := mustRate(t, "30000/1001")
	cases := map[string]int64{
		"00:00:00;00": 0,
		"00:01:00;02": 1800,  // 2 frame numbers dropped at minute 1
		"00:10:00;00": 17982, // 18 dropped over 10 minutes
		"01:00:00;00": 107892,
	}
	for s, want := range cases {
		tc, err := ParseTimecode(s, r, 0)
		if err != nil {
			t.Fatalf("%s: %v", s, err)
		}
		if tc.Frame != want {
			t.Errorf("%s: frame=%d want %d", s, tc.Frame, want)
		}
		if got := Format(tc.Nanos, r, true); got != s {
			t.Errorf("round-trip %s got %q", s, got)
		}
	}
	// Dropped frame numbers do not exist.
	if _, err := ParseTimecode("00:01:00;00", r, 0); err == nil {
		t.Error("00:01:00;00 should not exist in drop-frame")
	}
}

func TestIllegalAndAmbiguous(t *testing.T) {
	r30 := mustRate(t, "30")
	// Illegal frame number.
	if _, err := ParseTimecode("00:00:00:30", r30, 42); err == nil {
		t.Fatal("frame 30 at 30fps should fail")
	} else {
		fe, ok := err.(*fault.Error)
		if !ok || fe.Offset != 42 || fe.Cat != fault.CatTimecode {
			t.Errorf("error not locatable: %v", err)
		}
	}
	// Drop-frame separator with integer rate is ambiguous.
	if _, err := ParseTimecode("00:00:00;01", r30, 7); err == nil {
		t.Fatal("drop-frame at 30fps should be ambiguous")
	} else if fe := err.(*fault.Error); fe.Offset != 7 {
		t.Errorf("ambiguous error missing source offset: %v", fe)
	}
	if _, err := ParseTimecode("00:00:61:00", r30, 0); err == nil {
		t.Error("second=61 should fail")
	}
}

func TestWrapAndOffset(t *testing.T) {
	if got := WrapDay(DayNanos + 5); got != 5 {
		t.Errorf("wrap over day: %d", got)
	}
	if got := WrapDay(-1); got != DayNanos-1 {
		t.Errorf("wrap negative: %d", got)
	}
	// Negative offset crossing midnight wraps via WrapDay.
	v, err := ApplyOffset(100, -200)
	if err != nil {
		t.Fatal(err)
	}
	if WrapDay(v) != DayNanos-100 {
		t.Errorf("cross-midnight wrap: %d", WrapDay(v))
	}
	// Span beyond 48h rejected.
	if err := ValidateSpan(0, MaxSpanNanos+1, 0); err == nil {
		t.Error("span > 48h should fail")
	}
	if err := ValidateSpan(10, 5, 0); err == nil {
		t.Error("reversed span should fail")
	}
	if _, err := ApplyOffset(MaxSpanNanos, MaxSpanNanos); err == nil {
		t.Error("overflowing offset should fail")
	}
}

func TestErrorCarriesSourceOffset(t *testing.T) {
	r := mustRate(t, "25")
	_, err := ParseTimecode("garbage", r, 1234)
	fe, ok := err.(*fault.Error)
	if !ok {
		t.Fatalf("want *fault.Error, got %T", err)
	}
	if fe.Offset != 1234 || !strings.Contains(fe.Error(), "1234") {
		t.Errorf("offset not propagated: %v", fe)
	}
}
