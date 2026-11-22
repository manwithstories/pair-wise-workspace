package clock

import "testing"

func TestFrameToNanosExact(t *testing.T) {
	cases := []struct {
		r Rate
		f int64
	}{
		{FPS24, 24},
		{FPS25, 25},
		{FPS30, 30},
		{FPS2997, 30},
	}
	for _, c := range cases {
		got := c.r.FrameToNanos(c.f)
		if got <= 0 {
			t.Fatalf("rate %s frame %d: got %d", c.r, c.f, got)
		}
	}
	// 30000/1001: 30 frames must be exactly 1.001s, no float drift.
	if got := FPS2997.FrameToNanos(30); got != 1_001_000_000 {
		t.Fatalf("30000/1001 frame 30 = %d ns, want 1001000000", got)
	}
	if got := FPS2997.FrameToNanos(30000); got != 1001*NanosPerSec {
		t.Fatalf("30000/1001 frame 30000 = %d ns, want %d", got, 1001*NanosPerSec)
	}
	if got := FPS24.FrameToNanos(24); got != NanosPerSec {
		t.Fatalf("24fps frame 24 = %d ns, want 1e9", got)
	}
}

func TestFrameNanosRoundTripMonotonic(t *testing.T) {
	prev := int64(-1)
	for f := int64(0); f < 1_000_000; f += 997 {
		ns := FPS2997.FrameToNanos(f)
		if ns <= prev {
			t.Fatalf("not monotonic at frame %d: %d <= %d", f, ns, prev)
		}
		prev = ns
		if back := FPS2997.NanosToFrame(ns); back > f {
			t.Fatalf("round trip overshoot at frame %d: %d", f, back)
		}
	}
}

func TestParseTimecode(t *testing.T) {
	tc, err := ParseTimecode("00:00:10:00", FPS24)
	if err != nil {
		t.Fatal(err)
	}
	if tc.Nanos() != 10*NanosPerSec {
		t.Fatalf("got %d ns", tc.Nanos())
	}
	// 30000/1001 tail offset: one hour of timecode is not one hour.
	tc, err = ParseTimecode("01:00:00:00", FPS2997)
	if err != nil {
		t.Fatal(err)
	}
	want := FPS2997.FrameToNanos(30 * 3600)
	if tc.Nanos() != want {
		t.Fatalf("got %d, want %d", tc.Nanos(), want)
	}
}

func TestParseTimecodeRejects(t *testing.T) {
	bad := []struct {
		s string
		r Rate
	}{
		{"00:00:00:24", FPS24},   // illegal frame number
		{"00:00:00:30", FPS2997}, // nominal bound is 30
		{"00:00:60:00", FPS25},   // seconds out of range
		{"1:2:3:4", FPS25},       // malformed
		{"00:00:00:0a", FPS25},   // non-numeric
		{"00:00:00:00", Rate{15, 1}}, // unsupported rate
	}
	for _, b := range bad {
		if _, err := ParseTimecode(b.s, b.r); err == nil {
			t.Fatalf("ParseTimecode(%q, %s) unexpectedly succeeded", b.s, b.r)
		}
	}
	// Legal boundary frames must pass.
	if _, err := ParseTimecode("00:00:00:23", FPS24); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseTimecode("00:00:00:29", FPS2997); err != nil {
		t.Fatal(err)
	}
}
