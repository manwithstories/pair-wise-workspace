package clock

import "testing"

func TestParseAndRoundTrip(t *testing.T) {
	cases := []struct {
		label string
		rate  Rate
	}{
		{"00:00:00:00", Rate24},
		{"01:00:00:00", Rate25},
		{"00:00:01:00", Rate30},
		{"23:59:59:29", Rate2997},
	}
	for _, c := range cases {
		tc, err := Parse(c.label)
		if err != nil {
			t.Fatalf("Parse(%q): %v", c.label, err)
		}
		f, err := ToFrames(tc, c.rate)
		if err != nil {
			t.Fatalf("ToFrames(%q): %v", c.label, err)
		}
		back, err := FromFrames(f, c.rate)
		if err != nil {
			t.Fatalf("FromFrames(%d): %v", f, err)
		}
		if back.String() != c.label {
			t.Fatalf("round trip %q -> %d -> %q", c.label, f, back.String())
		}
	}
}

func TestParseRejectsInvalid(t *testing.T) {
	bad := []string{"12:60:00:00", "12:00:60:00", "24:00:00:00", "xx:00:00:00", "00:00:00", "00;00;00;00"}
	for _, b := range bad {
		if _, err := Parse(b); err == nil {
			t.Fatalf("Parse(%q) expected error, got nil", b)
		}
	}
}

func TestFrameFieldExceedsFPS(t *testing.T) {
	tc, _ := Parse("00:00:00:24")
	if _, err := ToFrames(tc, Rate24); err == nil {
		t.Fatal("frame 24 at 24fps must be rejected, not zeroed")
	}
}

func TestDropFrameKnownVectors(t *testing.T) {
	// SMPTE known vectors: 00:01:00;00 = frame 1798, 00:10:00;00 = 17982.
	vec := map[string]int64{
		"00:01:00;00": 1798,
		"00:10:00;00": 17982,
		"01:00:00;00": 107892,
	}
	for label, want := range vec {
		tc, err := Parse(label)
		if err != nil {
			t.Fatalf("Parse %q: %v", label, err)
		}
		got, err := ToFrames(tc, Rate2997DF)
		if err != nil {
			t.Fatalf("ToFrames %q: %v", label, err)
		}
		if got != want {
			t.Fatalf("%s = %d, want %d", label, got, want)
		}
		back, err := FromFrames(want, Rate2997DF)
		if err != nil {
			t.Fatalf("FromFrames %d: %v", want, err)
		}
		if back.String() != label {
			t.Fatalf("drop round trip %s -> %s", label, back.String())
		}
	}
}

func TestDropFrameSkipsZeroOne(t *testing.T) {
	tc, _ := Parse("00:01:00;01") // dropped label at non-tenth minute
	if _, err := ToFrames(tc, Rate2997DF); err == nil {
		t.Fatal("dropped frame label must be rejected")
	}
	// Flag mismatch must be an explicit error.
	nd, _ := Parse("00:00:00:00")
	if _, err := ToFrames(nd, Rate2997DF); err == nil {
		t.Fatal("non-drop label against drop rate must error")
	}
}

func TestCrossMidnightWrap(t *testing.T) {
	first, _ := Parse("23:59:59:24")
	f1, err := ToFrames(first, Rate25)
	if err != nil {
		t.Fatal(err)
	}
	next, _ := Parse("00:00:00:00")
	f2, err := ResolveWrap(next, Rate25, f1)
	if err != nil {
		t.Fatalf("wrap resolution failed: %v", err)
	}
	if f2 != int64(25*3600*24) {
		t.Fatalf("wrapped frame = %d, want %d", f2, 25*3600*24)
	}
	tc, _ := FromFrames(f2, Rate25)
	if tc.Day != 1 || tc.Hour != 0 {
		t.Fatalf("expected day 1 00:00, got day %d %s", tc.Day, tc.String())
	}
}

func TestExactMillisNoDrift(t *testing.T) {
	// At 30000/1001, 30000 frames is exactly 1001000 ms.
	ms, err := FramesToMillis(30000, Rate2997)
	if err != nil {
		t.Fatal(err)
	}
	if ms != 1_001_000 {
		t.Fatalf("millis = %d, want 1001000", ms)
	}
}
