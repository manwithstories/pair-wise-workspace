package clock

import "testing"

func TestRationalFrameConversion(t *testing.T) {
	cases := []struct {
		name string
		r    Rate
		secs int64
		want int64
	}{
		{"24", Rate24, 1, 24},
		{"25", Rate25, 1, 25},
		{"30", Rate30, 1, 30},
		{"29.97-one-second-floor", Rate30000_1001, 1, 29},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ms := tc.secs * 1000
			gotFrames := tc.r.MillisToFrames(ms)
			if gotFrames != tc.want {
				t.Fatalf("MillisToFrames(%d)=%d, want %d", ms, gotFrames, tc.want)
			}
		})
	}
	if got := Rate30000_1001.FramesToMillis(30); got != 1001 {
		t.Fatalf("30 NTSC frames = %d ms, want 1001", got)
	}
}

func TestDropFrameSkipsNumbers(t *testing.T) {
	valid, err := ParseTimecode("00:10:00;00", Rate30000_1001, true)
	if err != nil {
		t.Fatal(err)
	}
	if valid.FrameIndex() <= 0 {
		t.Fatalf("expected positive frame index: %+v", valid)
	}
	for _, raw := range []string{"00:01:00;00", "00:01:00;01", "01:09:00;01"} {
		if _, err := ParseTimecode(raw, Rate30000_1001, true); err == nil {
			t.Fatalf("expected illegal DF error for %s", raw)
		}
	}
	if _, err := ParseTimecode("00:01:00:00", Rate25, true); err == nil {
		t.Fatal("DF on 25 fps must be rejected")
	}
}

func TestCrossMidnightAndRegression(t *testing.T) {
	prev, err := ParseTimecode("23:59:59:29", Rate30, false)
	if err != nil {
		t.Fatal(err)
	}
	next, err := ParseTimecode("00:00:00:01", Rate30, false)
	if err != nil {
		t.Fatal(err)
	}
	tr, err := CheckTransition(prev, next)
	if err != nil || !tr.CrossMidnight {
		t.Fatalf("expected midnight wrap, tr=%+v err=%v", tr, err)
	}

	badPrev, _ := ParseTimecode("01:00:00:00", Rate30, false)
	badNext, _ := ParseTimecode("00:59:59:29", Rate30, false)
	if _, err := CheckTransition(badPrev, badNext); err == nil {
		t.Fatal("expected timecode regression")
	}
}

func TestLTCVITCAlignment(t *testing.T) {
	ltc, _ := ParseTimecode("01:00:00:00", Rate25, false)
	vitc, _ := ParseTimecode("01:00:00:01", Rate25, false)
	a, err := CheckAlignment(ltc, vitc, 1)
	if err != nil || !a.Aligned || a.DeltaFrames != 1 {
		t.Fatalf("unexpected alignment: %+v err=%v", a, err)
	}
	vitc.F = 3
	if err := vitc.Validate(); err != nil {
		t.Fatal(err)
	}
	a, _ = CheckAlignment(ltc, vitc, 1)
	if a.Aligned {
		t.Fatal("3-frame VITC skew should exceed tolerance")
	}
}
