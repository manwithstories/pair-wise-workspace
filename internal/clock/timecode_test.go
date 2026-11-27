package clock

import "testing"

func TestRateFrameDurationExact(t *testing.T) {
	cases := []struct {
		r             Rate
		frames        int64
		wantTickPerFr int64
	}{
		{Rate24, 1, 3750},
		{Rate25, 1, 3600},
		{Rate30, 1, 3000},
		{Rate2997, 1, 3003}, // 90000*30000/1001
	}
	for _, c := range cases {
		if got := c.r.FrameDuration(c.frames); got != c.wantTickPerFr {
			t.Errorf("%v: one frame = %d ticks, want %d", c.r, got, c.wantTickPerFr)
		}
	}
	// No cumulative drift: every frame carries the same exact integer tick
	// count, so N successive frames equal N times one frame (no float
	// accumulation anywhere in the chain).
	one := Rate2997.FrameDuration(1)
	for _, n := range []int64{1, 100, 1798, 17982, 100_000} {
		if got, want := Rate2997.FrameDuration(n), one*n; got != want {
			t.Errorf("frame*%d ticks = %d, want %d", n, got, want)
		}
	}
	// Exact integer frame rates fill a whole day without remainder.
	if Rate25.FrameDuration(Rate25.FramesPerDay()) != TicksPerSecond*86400 {
		t.Error("25 fps full day must equal exactly 86400 seconds")
	}
}

func TestParseRate(t *testing.T) {
	if _, err := ParseRate("29.97"); err == nil {
		t.Error("float rate spelling must be rejected")
	}
	r, err := ParseRate("30000/1001")
	if err != nil || r != Rate2997 {
		t.Errorf("rational rate parse: %v %v", r, err)
	}
}

func TestTimecodeRoundTrip(t *testing.T) {
	r := Rate2997
	addrs := []string{
		"00:00:00;00", "00:00:59;29", "00:01:00;02", "00:09:59;29",
		"00:10:00;00", "01:00:00;00", "10:00:03;28", "23:59:59;29",
	}
	for _, a := range addrs {
		tc, err := ParseTimecode(a, r)
		if err != nil {
			t.Fatalf("parse %q: %v", a, err)
		}
		frames, err := tc.frameCount()
		if err != nil {
			t.Fatalf("frame count %q: %v", a, err)
		}
		back, err := FromFrames(frames, r, true)
		if err != nil {
			t.Fatalf("from frames %q: %v", a, err)
		}
		if back.String() != tc.String() {
			t.Errorf("drop-frame round trip %q -> f=%d -> %q", a, frames, back.String())
		}
	}
}

func TestDropFrameSkips(t *testing.T) {
	r := Rate2997
	// First displayed frame after 00:00:59;29 is 00:01:00;02: real frame 1798.
	a, _ := ParseTimecode("00:00:59;29", r)
	b, _ := ParseTimecode("00:01:00;02", r)
	fa, _ := a.frameCount()
	fb, _ := b.frameCount()
	if fb-fa != 1 {
		t.Errorf("drop minute boundary gap = %d frames, want 1", fb-fa)
	}
	if fa != 1799 {
		t.Errorf("00:00:59;29 real frame = %d, want 1799", fa)
	}
	// Every tenth minute is not dropped: 00:09:59;29 -> 00:10:00;00 contiguous.
	c, _ := ParseTimecode("00:09:59;29", r)
	d, _ := ParseTimecode("00:10:00;00", r)
	fc, _ := c.frameCount()
	fd, _ := d.frameCount()
	if fd-fc != 1 {
		t.Errorf("tenth-minute boundary gap = %d frames, want 1", fd-fc)
	}
}

func TestNonDropRates(t *testing.T) {
	for _, r := range []Rate{Rate24, Rate25, Rate30} {
		tc := Timecode{Hour: 1, Minute: 2, Second: 3, Frame: 4, Rate: r}
		f, err := tc.frameCount()
		if err != nil {
			t.Fatal(err)
		}
		back, err := FromFrames(f, r, false)
		if err != nil {
			t.Fatal(err)
		}
		if back != tc {
			t.Errorf("%v round trip: %+v != %+v", r, back, tc)
		}
	}
	if _, err := ParseTimecode("00:00:00;00", Rate30); err == nil {
		t.Error("drop marker at non-NTSC rate must be rejected")
	}
}

func TestDayWrapAndNegativeShift(t *testing.T) {
	r := Rate25
	last, err := ParseTimecode("23:59:59:24", r)
	if err != nil {
		t.Fatal(err)
	}
	next, err := last.Shift(1)
	if err != nil {
		t.Fatal(err)
	}
	if next.Hour != 0 || next.Minute != 0 || next.Second != 0 || next.Frame != 0 {
		t.Errorf("midnight wrap failed: %s", next)
	}
	first, _ := ParseTimecode("00:00:00:00", r)
	prev, err := first.Shift(-1)
	if err != nil {
		t.Fatal(err)
	}
	if prev.String() != "23:59:59:24" {
		t.Errorf("negative wrap failed: %s", prev)
	}
	// Large negative offset across two day boundaries.
	back, err := first.Shift(-(r.FramesPerDay() + 25))
	if err != nil || back.String() != "23:59:59:00" {
		t.Errorf("multi-day negative shift: %s err=%v", back, err)
	}
}

func TestTicksConversions(t *testing.T) {
	if Millis(1000) != TicksPerSecond {
		t.Error("Millis conversion broken")
	}
	if FromSeconds(0.5) != TicksPerSecond/2 {
		t.Error("FromSeconds conversion broken")
	}
	if Seconds(TicksPerSecond*3) != 3.0 {
		t.Error("Seconds conversion broken")
	}
	tc, _ := ParseTimecode("00:00:01:00", Rate30)
	tk, err := tc.Ticks()
	if err != nil || tk != TicksPerSecond {
		t.Errorf("one second at 30fps = %d ticks, err=%v", tk, err)
	}
}
