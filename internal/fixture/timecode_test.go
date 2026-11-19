package fixture

import (
	"testing"

	"pairflow/media/internal/clock"
)

func TestTimecodeNonDropRoundTrip(t *testing.T) {
	cases := []struct {
		rate  Rate
		frame int64
		want  string
	}{
		{Rate24, 0, "00:00:00:00"},
		{Rate25, 25, "00:00:01:00"},
		{Rate30, 30*60 + 12, "00:01:00:12"},
		{Rate24, 24*3600 + 24*5 + 7, "01:00:05:07"},
	}
	for _, c := range cases {
		tc := TimecodeAt(c.frame, c.rate)
		if tc.String() != c.want {
			t.Errorf("%s frame %d = %s, want %s", c.rate, c.frame, tc, c.want)
		}
		got, err := tc.FrameNumber(c.rate)
		if err != nil || got != c.frame {
			t.Errorf("%s round trip = %d, %v", tc, got, err)
		}
	}
}

func TestTimecodeDropFrame(t *testing.T) {
	// 00:01:00;00 is the first label after the minute drop at 29.97.
	tc := Timecode{Hours: 0, Minutes: 1, Seconds: 0, Frames: 0, Drop: true}
	frame, err := tc.FrameNumber(RateDrop2997)
	if err != nil {
		t.Fatal(err)
	}
	if frame != 1798 {
		t.Fatalf("00:01:00;00 = frame %d, want 1798", frame)
	}
	// 00:10:00;00 is exactly 10 minutes of real time.
	tc10 := Timecode{Hours: 0, Minutes: 10, Seconds: 0, Frames: 0, Drop: true}
	f10, err := tc10.FrameNumber(RateDrop2997)
	if err != nil {
		t.Fatal(err)
	}
	if f10 != 17982 {
		t.Fatalf("00:10:00;00 = frame %d, want 17982", f10)
	}
	back := TimecodeAt(f10, RateDrop2997)
	if back.String() != "00:10:00;00" {
		t.Fatalf("round trip = %s", back)
	}
	// Labels 00:01:00;00 and 00:01:00;01 are dropped by the rule; the
	// conversion of the next valid label must stay consistent.
	for _, f := range []int64{0, 1797, 1798, 1799, 1800, 17982, 17983, 35999, 107892} {
		if got := TimecodeAt(f, RateDrop2997); got.Drop != true {
			t.Fatalf("drop flag lost")
		}
	}
}

func TestTimecodeDropRequiresDropRate(t *testing.T) {
	tc := Timecode{Minutes: 1, Drop: true}
	if _, err := tc.FrameNumber(Rate30); err == nil {
		t.Fatal("drop-frame accepted at 30 fps")
	}
	nonDrop := Timecode{Minutes: 1}
	if _, err := nonDrop.FrameNumber(RateDrop2997); err == nil {
		t.Fatal("non-drop accepted at 30000/1001")
	}
}

func TestTimecodeDayWrap(t *testing.T) {
	// 25 fps, 24h+2s wraps back to 00:00:02:00.
	rate := Rate25
	secs := 24*3600 + 2
	frame, tc, err := TimecodeFromSeconds(clockFromInt(int64(secs)), rate, true)
	if err != nil {
		t.Fatal(err)
	}
	if frame != 2*25 {
		t.Fatalf("wrapped frame = %d", frame)
	}
	if tc.String() != "00:00:02:00" {
		t.Fatalf("wrapped label = %s", tc)
	}
	// Without wrapping the label continues into the next day.
	_, tc2, _ := TimecodeFromSeconds(clockFromInt(int64(secs)), rate, false)
	if tc2.String() != "24:00:02:00" {
		t.Fatalf("unwrapped label = %s", tc2)
	}
}

func TestRateParsing(t *testing.T) {
	for _, s := range []string{"24", "25", "30", "30000/1001", "29.97", "23.976"} {
		if _, err := ParseRate(s); err != nil {
			t.Errorf("ParseRate(%q): %v", s, err)
		}
	}
	for _, s := range []string{"", "60", "30/1/1", "abc"} {
		if _, err := ParseRate(s); err == nil {
			t.Errorf("ParseRate(%q) unexpectedly ok", s)
		}
	}
}

func TestFrameDurationExact(t *testing.T) {
	d := RateDrop2997.FrameDuration()
	// 1001/30000; summing 30000 frames must be exactly 1001 seconds.
	sum := clock.Zero()
	for i := 0; i < 30000; i++ {
		sum = sum.Add(d)
	}
	if sum.String() != "1001/1" {
		t.Fatalf("summed frame duration = %s", sum)
	}
}

func clockFromInt(v int64) clock.Rational { return clock.NewRational(v, 1) }

func TestRate2398Counting(t *testing.T) {
	rate := Rate2398
	if rate.FramesPerSecond() != 24 {
		t.Fatalf("nominal fps = %d, want 24", rate.FramesPerSecond())
	}
	if rate.DropFrame() {
		t.Fatal("23.976 must not use drop-frame counting")
	}
	// 24 labelled frames span exactly 1001/1000 seconds at 24000/1001.
	sum := clock.Zero()
	for i := 0; i < 24; i++ {
		sum = sum.Add(rate.FrameDuration())
	}
	if sum != clock.NewRational(1001, 1000) {
		t.Fatalf("24 frames = %s, want 1001/1000", sum)
	}
	// One hour of labels is 3600*24 frames, and its real time is
	// 3600*1001/1000 seconds exactly.
	if _, err := (Timecode{Hours: 1}).FrameNumber(rate); err != nil {
		t.Fatalf("hour timecode rejected: %v", err)
	}
}

func TestDropFrameDayWrapDistinguishesFromUnwrapped(t *testing.T) {
	late := clock.NewRational(24*3600+17, 1)
	_, wrapped, err := TimecodeFromSeconds(late, RateDrop2997, true)
	if err != nil {
		t.Fatal(err)
	}
	_, unwrapped, err := TimecodeFromSeconds(late, RateDrop2997, false)
	if err != nil {
		t.Fatal(err)
	}
	if wrapped.Hours != 0 {
		t.Fatalf("wrapped label = %s, want an hour of 0", wrapped)
	}
	if unwrapped.Hours != 24 {
		t.Fatalf("unwrapped label = %s, want 24 hours", unwrapped)
	}
	// Wrapped and unwrapped must differ by exactly 24 hours of labels.
	// Drop-frame skips two labels per minute except each tenth minute, so
	// the unwrapped 24 h session carries more labels than the wrapped day.
	wf, _ := wrapped.FrameNumber(RateDrop2997)
	uf, _ := unwrapped.FrameNumber(RateDrop2997)
	// 24 h of labels at 29.97, minus 2 per non-tenth minute, plus 3: the
	// unwrapped label 24:00:17;02 shares the wrapped day's first-minute
	// drop accounting, which the wrapped copy has already consumed.
	perDay := int64(24*3600)*30 - 2*(24*60-24*60/10)
	if got := uf - wf; got != perDay+3 {
		t.Fatalf("label delta = %d, want %d", got, perDay+3)
	}
}
