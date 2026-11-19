package timeline_test

import (
	"math"
	"testing"

	"mediasync/internal/media"
	"mediasync/internal/timeline"
)

func TestParseRate(t *testing.T) {
	cases := []struct {
		in   string
		want timeline.Rate
	}{
		{"25", timeline.Rate25},
		{"30000/1001", timeline.Rate29_97},
		{"29.97", timeline.Rate29_97},
		{"23.976", timeline.Rate23_976},
		{"59.94", timeline.Rate59_94},
		{"50/2", timeline.Rate25},
		{"24", timeline.Rate24},
	}
	for _, c := range cases {
		got, err := timeline.ParseRate(c.in)
		if err != nil {
			t.Fatalf("ParseRate(%q): %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("ParseRate(%q) = %v, want %v", c.in, got, c.want)
		}
	}
	for _, bad := range []string{"", "abc", "0", "-25", "25/0", "1/2/3", "NaN"} {
		if _, err := timeline.ParseRate(bad); err == nil {
			t.Errorf("ParseRate(%q) should fail", bad)
		} else if ie, ok := err.(*media.Issue); !ok || ie.Stage != media.StageTimebase {
			t.Errorf("ParseRate(%q) error lacks stage: %v", bad, err)
		}
	}
}

func TestFrameConversions(t *testing.T) {
	for _, rate := range []timeline.Rate{timeline.Rate23_976, timeline.Rate25, timeline.Rate29_97, timeline.Rate59_94} {
		for _, frames := range []int64{0, 1, 7, 1000, 90000, 8639999} {
			ns := rate.FramesToNanos(frames)
			back := rate.NanosToFrames(ns)
			if back != frames {
				t.Errorf("%v: frames %d -> %dns -> %d", rate, frames, ns, back)
			}
			if ns < 0 {
				t.Errorf("%v: negative nanos for %d frames", rate, frames)
			}
		}
	}
}

func TestRoundtripConsistencySweep(t *testing.T) {
	rate := timeline.Rate29_97
	good := 0
	const total = 20000
	for f := int64(0); f < total; f++ {
		if rate.NanosToFrames(rate.FramesToNanos(f)) == f {
			good++
		}
	}
	if ratio := float64(good) / total; ratio < 0.99 {
		t.Errorf("roundtrip ratio %.4f below 0.99", ratio)
	}
}

func TestClockConversions(t *testing.T) {
	tb, err := timeline.NewTimebase(timeline.Rate25, timeline.Clock{Domain: "test", Ticks: 90000})
	if err != nil {
		t.Fatal(err)
	}
	if got := tb.TicksToFrames(90000); got != 25 {
		t.Errorf("90000 ticks at 25fps = %d frames, want 25", got)
	}
	if got := tb.FramesToTicks(25); got != 90000 {
		t.Errorf("25 frames = %d ticks, want 90000", got)
	}
	if got := tb.TicksToNanos(72000); got != 800_000_000 {
		t.Errorf("72000 ticks = %d ns, want 800000000", got)
	}
}

func TestCompatible(t *testing.T) {
	a, _ := timeline.NewTimebase(timeline.Rate25, timeline.Clock{Domain: "cam-a", Ticks: 90000})
	b, _ := timeline.NewTimebase(timeline.Rate25, timeline.Clock{Domain: "cam-a", Ticks: 90000})
	if !a.Compatible(b) {
		t.Error("identical timebases must be compatible")
	}
	// Different frame rates still describe the same instants once mapped onto
	// the shared timeline, so content can be compared.
	c, _ := timeline.NewTimebase(timeline.Rate29_97, timeline.Clock{Domain: "cam-a", Ticks: 90000})
	if !a.Compatible(c) {
		t.Error("different frame rates with valid clocks must remain comparable")
	}
	bad, _ := timeline.NewTimebase(timeline.Rate25, timeline.Clock{Domain: "x", Ticks: 90000})
	bad.Rate = timeline.Rate{0, 1}
	if a.Compatible(bad) {
		t.Error("a timebase with an invalid rate must not be compatible")
	}
	zero, _ := timeline.NewTimebase(timeline.Rate25, timeline.Clock{Domain: "z", Ticks: 90000})
	zero.Clock.Ticks = 0
	if a.Compatible(zero) {
		t.Error("a timebase with zero tick rate must not be compatible")
	}
	wrapped, _ := timeline.NewTimebase(timeline.Rate25, timeline.Clock{Domain: "w", Ticks: 90000, Wrap: 1 << 16})
	if a.Compatible(wrapped) {
		t.Error("a different wrap modulus must not be compatible")
	}
	// A different clock *source* still maps onto the same timeline, so it
	// must remain comparable across cameras.
	d, _ := timeline.NewTimebase(timeline.Rate25, timeline.Clock{Domain: "cam-b", Ticks: 90000})
	if !a.Compatible(d) {
		t.Error("same rate/ticks must stay comparable across clock domains")
	}
	// A different tick rate is still convertible onto the timeline; it must
	// not block comparison by itself.
	e, _ := timeline.NewTimebase(timeline.Rate25, timeline.Clock{Domain: "cam-a", Ticks: 48000})
	if !a.Compatible(e) {
		t.Error("a valid but different tick rate must remain comparable")
	}
}

func TestParseTimecode(t *testing.T) {
	cases := []struct {
		rate  timeline.Rate
		label string
		want  int64
	}{
		{timeline.Rate25, "00:00:01:00", 25},
		{timeline.Rate25, "01:00:00:00", 90000},
		{timeline.Rate29_97, "00:00:00;00", 0},
		{timeline.Rate29_97, "00:01:00;02", 1800},
		{timeline.Rate24, "23:59:59:23", 2073599},
	}
	for _, c := range cases {
		got, err := timeline.ParseTimecode(c.label, c.rate)
		if err != nil {
			t.Fatalf("ParseTimecode(%q, %v): %v", c.label, c.rate, err)
		}
		if got.Frames != c.want {
			t.Errorf("ParseTimecode(%q, %v) = %d, want %d", c.label, c.rate, got.Frames, c.want)
		}
	}
	bad := []struct {
		rate  timeline.Rate
		label string
	}{
		{timeline.Rate24, "00:00:00:24"},
		{timeline.Rate25, "00:60:00:00"},
		{timeline.Rate25, "ab:cd:ef:gh"},
		{timeline.Rate25, "00:00:00"},
		{timeline.Rate25, "00:00:00;00"}, // drop-frame at 25 fps is illegal
		{timeline.Rate29_97, "00:01:00;00"},
	}
	for _, c := range bad {
		if _, err := timeline.ParseTimecode(c.label, c.rate); err == nil {
			t.Errorf("ParseTimecode(%q, %v) should fail", c.label, c.rate)
		}
	}
}

func TestTimecodeRollover(t *testing.T) {
	u := timeline.NewUnwrapper(timeline.Rate25)
	day := timeline.DayFrames(timeline.Rate25)
	seq := []int64{day - 2, day - 1, 0, 1, 2}
	want := []int64{day - 2, day - 1, day, day + 1, day + 2}
	for i, in := range seq {
		out, wrapped := u.Unwrap(in)
		if out != want[i] {
			t.Errorf("unwrap(%d) = %d, want %d", in, out, want[i])
		}
		if i == 2 && !wrapped {
			t.Errorf("day boundary at index %d not reported", i)
		}
	}
}

func TestDuplicateTimecodeNotRollover(t *testing.T) {
	u := timeline.NewUnwrapper(timeline.Rate29_97)
	first, _ := u.Unwrap(100)
	dup, wrapped := u.Unwrap(100)
	if dup != first || wrapped {
		t.Errorf("duplicate timestamp must pass through unchanged, got %d wrapped=%v", dup, wrapped)
	}
}

func TestTickUnwrapper(t *testing.T) {
	u := timeline.NewTickUnwrapper(0)
	seq := []uint64{0xFFFFF000, 0xFFFFFF00, 0x00000100, 0x00001000}
	var last int64 = -1
	for _, v := range seq {
		out := u.Expand(v)
		if out <= last {
			t.Fatalf("tick %#x expanded to %d, not increasing after %d", v, out, last)
		}
		last = out
	}
}

func TestParseClockTimestamp(t *testing.T) {
	cases := map[string]int64{
		"00:00:01,500": 1_500_000_000,
		"00:00:01.500": 1_500_000_000,
		"01:02:03.004": 3_723_004_000_000,
		"02:03.250":    123_250_000_000,
		"00:00:00,000": 0,
	}
	for in, want := range cases {
		got, err := timeline.ParseClockTimestamp(in)
		if err != nil {
			t.Fatalf("ParseClockTimestamp(%q): %v", in, err)
		}
		if got != want {
			t.Errorf("ParseClockTimestamp(%q) = %d, want %d", in, got, want)
		}
	}
	for _, bad := range []string{"", "abc", "00:61:00,000", "00:00:00,xyz", "1:2:3:4"} {
		if _, err := timeline.ParseClockTimestamp(bad); err == nil {
			t.Errorf("ParseClockTimestamp(%q) should fail", bad)
		}
	}
}

func TestFormatClockTimestamp(t *testing.T) {
	if got := timeline.FormatClockTimestamp(3_723_004_000_000); got != "01:02:03.004" {
		t.Errorf("format = %q", got)
	}
	// Negative input is clamped rather than producing a negative clock.
	if got := timeline.FormatClockTimestamp(-1); got != "00:00:00.000" {
		t.Errorf("negative format = %q", got)
	}
}

func TestMulDivSaturation(t *testing.T) {
	rate, _ := timeline.ParseRate("1000000/1")
	if got := rate.FramesToNanos(math.MaxInt64); got < 0 {
		t.Errorf("saturating conversion returned negative value %d", got)
	}
}
