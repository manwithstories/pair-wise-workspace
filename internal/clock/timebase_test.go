package clock

import "testing"

func TestParseRate(t *testing.T) {
	for _, s := range []string{"24", "25", "30", "30000/1001", "29.97"} {
		if _, err := ParseRate(s); err != nil {
			t.Fatalf("ParseRate(%q): %v", s, err)
		}
	}
	if _, err := ParseRate("23.976"); err == nil {
		t.Fatal("expected error for unsupported rate")
	}
}

func TestSecondsFramesRoundTrip(t *testing.T) {
	for _, r := range []Rate{Rate24, Rate25, Rate30, Rate2997} {
		for _, f := range []int64{0, 1, 100, 123456} {
			if got := r.SecondsToFrames(r.FramesToSeconds(f)); got != f {
				t.Fatalf("%s: frame %d round-trip got %d", r, f, got)
			}
		}
	}
}

func TestDropFrameRoundTrip(t *testing.T) {
	for _, f := range []int64{0, 1, 1798, 1799, 1800, 1801, 107892, 2589407} {
		tc, err := FormatTimecode(f, Rate2997, true)
		if err != nil {
			t.Fatalf("format %d: %v", f, err)
		}
		parsed, err := ParseTimecode(tc)
		if err != nil {
			t.Fatalf("parse %q: %v", tc, err)
		}
		back, err := parsed.ToFrames(Rate2997)
		if err != nil {
			t.Fatalf("to-frames %q: %v", tc, err)
		}
		if back != f {
			t.Fatalf("drop-frame round-trip %d -> %q -> %d", f, tc, back)
		}
	}
	// Known label: frame 1800 is 00:01:00;02 at 29.97 DF.
	tc, _ := FormatTimecode(1800, Rate2997, true)
	if tc != "00:01:00;02" {
		t.Fatalf("frame 1800 = %q, want 00:01:00;02", tc)
	}
}

func TestDroppedLabelRejected(t *testing.T) {
	tc, err := ParseTimecode("00:01:00;00")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tc.ToFrames(Rate2997); err == nil {
		t.Fatal("expected dropped-label error")
	}
}

func TestDropUnsupportedRate(t *testing.T) {
	tc, _ := ParseTimecode("01:00:00;00")
	if _, err := tc.ToFrames(Rate25); err == nil {
		t.Fatal("expected drop-frame unsupported error at 25fps")
	}
}

func TestDayWrap(t *testing.T) {
	r := Rate2997
	day := r.FramesPerDay(true)
	if day != 2589408 {
		t.Fatalf("DF frames per day = %d, want 2589408", day)
	}
	if !r.DayWraps(day-30, 10, true) {
		t.Fatal("expected wrap detection past midnight")
	}
	if r.DayWraps(100, 200, true) {
		t.Fatal("false wrap detection")
	}
	// Timecode with day offset converts past one full day.
	tc, _ := ParseTimecode("00:00:01;00")
	tc.Day = 1
	f, err := tc.ToFrames(r)
	if err != nil {
		t.Fatal(err)
	}
	if f != day+30 {
		t.Fatalf("day-offset frames = %d, want %d", f, day+30)
	}
}
