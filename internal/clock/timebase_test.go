package clock

import (
	"strings"
	"testing"
	"time"
)

func mustTB(t *testing.T, fps string) Timebase {
	t.Helper()
	r, err := ParseRational(fps)
	if err != nil {
		t.Fatalf("ParseRational(%q): %v", fps, err)
	}
	return Timebase{FPS: r}
}

func TestParseRationalSupported(t *testing.T) {
	for _, s := range []string{"24", "25", "30", "30000/1001"} {
		if !Supported(s) {
			t.Fatalf("Supported(%q) = false", s)
		}
		if _, err := ParseRational(s); err != nil {
			t.Fatalf("ParseRational(%q): %v", s, err)
		}
	}
	if Supported("24000/1001") {
		t.Fatal("24000/1001 must not be supported")
	}
	if _, err := ParseRational("30/0"); err == nil {
		t.Fatal("expected error for zero denominator")
	}
}

func TestFrameDurationRoundTrip(t *testing.T) {
	tb := mustTB(t, "30000/1001")
	for _, f := range []int64{0, 1, 60, 17982, 2589407} {
		d := tb.FramesToDuration(f)
		got := tb.DurationToFrames(d)
		if got != f {
			t.Fatalf("frame %d -> %v -> %d", f, d, got)
		}
	}
	// 10 minutes at 30000/1001 fps.
	if got := tb.DurationToFrames(10 * time.Minute); got != 17982 {
		t.Fatalf("10min = %d frames, want 17982", got)
	}
}

func TestTimecodeRoundTrip(t *testing.T) {
	tb := mustTB(t, "30000/1001")
	f, err := tb.ParseTimecode("23:59:59:29")
	if err != nil {
		t.Fatal(err)
	}
	if got := tb.FormatTimecode(f); got != "23:59:59:29" {
		t.Fatalf("round trip = %q", got)
	}
	if _, err := tb.ParseTimecode("00:61:00:00"); err == nil {
		t.Fatal("expected range error")
	}
	var cerr *Error
	if e := func() error { _, err := tb.ParseTimecode("bad"); return err }(); e == nil ||
		!strings.Contains(e.Error(), "clock.ParseTimecode") || !asErr(e, &cerr) {
		t.Fatalf("want *clock.Error, got %v", e)
	}
}

func asErr(e error, target **Error) bool {
	ce, ok := e.(*Error)
	if ok {
		*target = ce
	}
	return ok
}

func TestCrossDayWrap(t *testing.T) {
	tb := mustTB(t, "30000/1001")
	day := tb.FramesPerDay()
	last, err := tb.ParseTimecode("23:59:59:29")
	if err != nil {
		t.Fatal(err)
	}
	next := last + 2 // crosses midnight
	if tb.WrapDay(next) != next-day {
		t.Fatalf("WrapDay did not wrap: %d day=%d", next, day)
	}
	// Unwrapped stream stays monotonic across midnight.
	if got := tb.UnwrapDay(last, tb.WrapDay(next)); got != next {
		t.Fatalf("UnwrapDay = %d, want %d", got, next)
	}
	if got := tb.FormatTimecode(next); got != "00:00:00:01" {
		t.Fatalf("FormatTimecode across day = %q", got)
	}
}

func TestRTPConversion(t *testing.T) {
	tb := mustTB(t, "30000/1001")
	baseRTP := uint32(100000)
	for _, f := range []int64{0, 1, 60, 17982} {
		rtp := tb.FrameToRTP(f, 0, baseRTP)
		got := tb.RTPToFrame(rtp, baseRTP, 0)
		if got != f {
			t.Fatalf("frame %d -> rtp %d -> %d", f, rtp, got)
		}
	}
	// RTP uint32 wraparound must still convert correctly.
	wrapped := tb.RTPToFrame(baseRTP-2*RTPClockRate, baseRTP, 100)
	if wrapped >= 100 {
		t.Fatalf("wrapped RTP conversion = %d, want < 100", wrapped)
	}
}
