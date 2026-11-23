package timeline

import (
	"testing"

	"mediad/diag"
)

func TestParseAndFrameRates(t *testing.T) {
	cases := []struct {
		name string
		fr   FrameRate
	}{
		{"24", FR24}, {"25", FR25}, {"30", FR30},
		{"30000/1001", FR2997}, {"30000/1001-df", FR2997DF},
	}
	for _, c := range cases {
		got, err := ParseFrameRate(c.name)
		if err != nil || got.Name != c.fr.Name || got.Drop != c.fr.Drop {
			t.Fatalf("ParseFrameRate(%q) = %+v, %v", c.name, got, err)
		}
	}
	if _, err := ParseFrameRate("120"); diag.CodeOf(err) != diag.ETimecode {
		t.Fatalf("unknown rate must be E_TIMECODE, got %v", err)
	}
}

func TestRatMillisRounding(t *testing.T) {
	// 29.97 frame 1 = 1001/30000 s = 33.366... ms -> rounds to 33.
	tt, err := FrameToTime(FR2997, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := tt.Millis(); got != 33 {
		t.Fatalf("frame1 ms = %d, want 33", got)
	}
	whole, _, _ := tt.FloorMillis()
	if whole != 33 {
		t.Fatalf("floor = %d, want 33", whole)
	}
	// Rational exactness: 30000 frames == 1001 s with zero residual.
	tt, _ = FrameToTime(FR2997, 30000)
	if tt.Cmp(NewRat(1001, 1)) != 0 {
		t.Fatalf("30000 NTSC frames = %s, want 1001s", tt.String())
	}
}

func TestFrameTimeRoundTripAllRates(t *testing.T) {
	for _, fr := range []FrameRate{FR24, FR25, FR30, FR2997} {
		for _, f := range []int64{0, 1, 999, 123456} {
			tt, err := FrameToTime(fr, f)
			if err != nil {
				t.Fatal(err)
			}
			back, err := TimeToFrame(fr, tt)
			if err != nil || back != f {
				t.Fatalf("%s round-trip frame %d -> %s -> %d, err=%v", fr.Name, f, tt, back, err)
			}
		}
	}
	if _, err := FrameToTime(FR30, -1); diag.CodeOf(err) != diag.ETimecode {
		t.Fatalf("negative frame must fail, got %v", err)
	}
}

func TestDropFrameForward(t *testing.T) {
	// Canonical SMPTE anchors.
	anchors := map[string]int64{
		"00:00:00;00": 0,
		"00:00:59;29": 1799, // minute 0 drops nothing: 59*30+29
		"00:01:00;02": 1800,
		"00:09:59;29": 17981, // 9*1800+59*30+29 - 18 dropped
		"00:10:00;00": 17982,
		"01:00:00;00": 107892,
	}
	for label, want := range anchors {
		tc, err := ParseTimecode(label, FR2997DF)
		if err != nil {
			t.Fatalf("parse %s: %v", label, err)
		}
		got, err := tc.FrameIndex(FR2997DF)
		if err != nil || got != want {
			t.Fatalf("%s -> frame %d, want %d (err=%v)", label, got, want, err)
		}
	}
}

func TestDropFrameFullDayRoundTrip(t *testing.T) {
	// Every real frame in a 29.97 day must map label->frame->label identically.
	day := FramesPer24h(FR2997DF)
	if day != 24*107892 {
		t.Fatalf("day frames = %d", day)
	}
	for idx := int64(0); idx < day; idx += 37 {
		tc := dropFrameLabel(idx)
		back, err := tc.FrameIndex(FR2997DF)
		if err != nil {
			t.Fatalf("label %s invalid: %v", tc, err)
		}
		if back != idx {
			t.Fatalf("round-trip mismatch at %d: %s -> %d", idx, tc, back)
		}
	}
	// Explicit check at the final frame of the day.
	last := dropFrameLabel(day - 1)
	if last.String() != "23:59:59;29" {
		t.Fatalf("last label = %s", last)
	}
}

func TestIllegalDropFrameNumbers(t *testing.T) {
	for _, bad := range []string{"00:01:00;00", "00:01:00;01", "00:05:00;01"} {
		if _, err := ParseTimecode(bad, FR2997DF); diag.CodeOf(err) != diag.ETimecode {
			t.Fatalf("%s must be rejected, got %v", bad, err)
		}
	}
	// Ten-minute boundary labels are legal.
	if _, err := ParseTimecode("00:10:00;00", FR2997DF); err != nil {
		t.Fatalf("ten-minute boundary must be legal: %v", err)
	}
}

func TestIllegalTimecodeComponents(t *testing.T) {
	bad := []string{"25:00:00:00", "00:60:00:00", "00:00:60:00", "00:00:00:30", "garbage"}
	for _, s := range bad {
		if _, err := ParseTimecode(s, FR30); diag.CodeOf(err) != diag.ETimecode {
			t.Fatalf("%q must be E_TIMECODE, got %v", s, err)
		}
	}
}

func TestDayBoundaryWraparound(t *testing.T) {
	day := FramesPer24h(FR2997DF)
	// One frame past midnight wraps back to 00:00:00;00 with wrap count 1.
	tc, err := FrameToTimecode(day, FR2997DF)
	if err != nil || tc.String() != "00:00:00;00" {
		t.Fatalf("wrap label = %s, err=%v", tc, err)
	}
	if w := WrapCount(day+5, FR2997DF); w != 1 {
		t.Fatalf("wrap count = %d, want 1", w)
	}
	// Non-drop 25fps boundary.
	tc25, err := FrameToTimecode(25*3600*24, FR25)
	if err != nil || tc25.String() != "00:00:00:00" {
		t.Fatalf("25fps wrap = %s, err=%v", tc25, err)
	}
}

func TestShiftFrameNegative(t *testing.T) {
	day := FramesPer24h(FR30)
	// Two frames before midnight, shifted back five -> previous day, wraps -1.
	idx, wraps := ShiftFrame(2, -5, FR30)
	if wraps != -1 || idx != day-3 {
		t.Fatalf("negative shift = (%d, %d), want (%d, -1)", idx, wraps, day-3)
	}
	// Forward shift across the boundary.
	idx, wraps = ShiftFrame(day-2, 5, FR30)
	if wraps != 1 || idx != 3 {
		t.Fatalf("forward shift = (%d, %d), want (3, 1)", idx, wraps)
	}
}
