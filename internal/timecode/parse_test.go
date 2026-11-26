package timecode

import "testing"

func TestParseFPS(t *testing.T) {
	for s, want := range map[string]FPS{
		"24": FPS24, "25": FPS25, "30": FPS30, "29.97": FPS2997, "30000/1001": FPS2997,
	} {
		got, err := ParseFPS(s)
		if err != nil || got != want {
			t.Errorf("ParseFPS(%q) = %v, %v; want %v", s, got, err, want)
		}
	}
	if _, err := ParseFPS("23.98"); err == nil {
		t.Error("ParseFPS(23.98) should fail")
	}
}

func TestParseNonDrop(t *testing.T) {
	f, err := Parse("01:00:00:00", FPS25, false)
	if err != nil || f != 90000 {
		t.Errorf("got %d, %v; want 90000", f, err)
	}
	f, err = Parse("23:59:59:23", FPS24, false)
	if err != nil || f != 24*86400-1 {
		t.Errorf("got %d, %v; want %d", f, err, 24*86400-1)
	}
	for _, bad := range []string{"24:00:00:00", "00:60:00:00", "00:00:60:00",
		"00:00:00:25", "1:00:00:00", "00:00:00;00"} {
		if _, err := Parse(bad, FPS25, false); err == nil {
			t.Errorf("Parse(%q) should fail", bad)
		}
	}
}

func TestParseDropFrame(t *testing.T) {
	cases := map[string]int64{
		"00:00:00;00": 0,
		"00:01:00;02": 1800,  // first minute drops labels 00,01
		"00:10:00;00": 17982, // 10-minute boundary drops nothing
		"01:00:00;00": 107892,
	}
	for s, want := range cases {
		got, err := Parse(s, FPS2997, true)
		if err != nil || got != want {
			t.Errorf("Parse(%q) = %d, %v; want %d", s, got, err, want)
		}
	}
	// Dropped labels and separator mismatch must be rejected.
	for _, bad := range []string{"00:01:00;00", "00:01:00;01", "00:01:00:02"} {
		if _, err := Parse(bad, FPS2997, true); err == nil {
			t.Errorf("Parse(%q) should fail", bad)
		}
	}
	if _, err := Parse("00:00:01;00", FPS25, true); err == nil {
		t.Error("drop-frame with 25fps should fail")
	}
}

func TestFrameMsRoundTrip(t *testing.T) {
	for _, fps := range []FPS{FPS24, FPS25, FPS30, FPS2997} {
		for _, f := range []int64{0, 1, 1000, 107892} {
			if got := MsToFrames(FramesToMs(f, fps), fps); got != f {
				t.Errorf("fps %v frame %d round trip = %d", fps, f, got)
			}
		}
	}
	// 29.97: 1800 frames = 60.06 s.
	if ms := FramesToMs(1800, FPS2997); ms != 60060 {
		t.Errorf("1800 frames @29.97 = %dms, want 60060", ms)
	}
}

func TestResolveDailyWrap(t *testing.T) {
	// Reference 23:59:59.000; label 00:00:02:00 @25fps unwraps to next day.
	got, err := ResolveDaily(50, FPS25, 86399_000)
	if err != nil || got != 86402_000 {
		t.Errorf("got %d, %v; want 86402000", got, err)
	}
	// Same label near noon stays same-day.
	got, err = ResolveDaily(50, FPS25, 12*3600_000)
	if err != nil || got != 2000 {
		t.Errorf("got %d, %v; want 2000", got, err)
	}
	// Exactly 12h away is ambiguous.
	if _, err := ResolveDaily(0, FPS25, 12*3600_000); err == nil {
		t.Error("half-day ambiguity should be rejected")
	}
}
