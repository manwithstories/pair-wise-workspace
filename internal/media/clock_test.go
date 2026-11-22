package media

import (
	"errors"
	"testing"
)

func TestParseFrameRate(t *testing.T) {
	cases := []struct {
		in      string
		label   string
		nominal int64
		drop    bool
	}{
		{"24", "24/1", 24, false},
		{"25", "25/1", 25, false},
		{"30", "30/1", 30, false},
		{"30000/1001", "30000/1001", 30, false},
		{"30000/1001df", "30000/1001df", 30, true},
		{"29.97", "", 0, false}, // deliberately rejected
		{"0", "", 0, false},
		{"25df", "", 0, false},
	}
	for _, c := range cases {
		got, err := ParseFrameRate(c.in)
		if c.label == "" {
			if err == nil {
				t.Errorf("ParseFrameRate(%q) = %v, want error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseFrameRate(%q): unexpected error %v", c.in, err)
			continue
		}
		if got.Label() != c.label {
			t.Errorf("ParseFrameRate(%q).Label() = %q, want %q", c.in, got.Label(), c.label)
		}
		if got.NominalFPS() != c.nominal {
			t.Errorf("ParseFrameRate(%q).NominalFPS() = %d, want %d", c.in, got.NominalFPS(), c.nominal)
		}
		if got.DropFrame != c.drop {
			t.Errorf("ParseFrameRate(%q).DropFrame = %v, want %v", c.in, got.DropFrame, c.drop)
		}
	}
}

func TestFrameToNanosExact29_97(t *testing.T) {
	// 30000/1001: frame 30000 is exactly 1001 seconds (well, 1001s) only in
	// the rational sense: 30000 * 1e9 * 1001 / 30000 = 1e9*1001 ns.
	ns, err := Rate2997.FrameToNanos(30000)
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(1001_000_000_000); ns != want {
		t.Fatalf("frame 30000 -> %d ns, want %d", ns, want)
	}
	// No float drift: 1001 frames at 29.97 is 1001e9*1001/30000 ns, floored
	// to 33400033333 ns exactly (the ideal is 33400033333.33).
	ns, err = Rate2997.FrameToNanos(1001)
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(33_400_033_333); ns != want {
		t.Fatalf("frame 1001 -> %d ns, want %d", ns, want)
	}
}

func TestNanosToFrameRoundTrip(t *testing.T) {
	rates := []FrameRate{Rate24, Rate25, Rate30, Rate2997,
		{Rational: Rate2997.Rational, DropFrame: false}}
	for _, r := range rates {
		for _, frame := range []int64{0, 1, 23, 24, 25, 1000, 29999, 30000, 179999} {
			ns, err := r.FrameToNanos(frame)
			if err != nil {
				t.Fatalf("%s frame %d: %v", r.Label(), frame, err)
			}
			back, rem, err := r.NanosToFrame(ns)
			if err != nil {
				t.Fatalf("%s ns %d: %v", r.Label(), ns, err)
			}
			if back != frame || rem != 0 {
				t.Errorf("%s frame %d -> %d ns -> frame %d rem %d", r.Label(), frame, ns, back, rem)
			}
		}
	}
}

func TestTimecodeDropFrameBoundaries(t *testing.T) {
	// Known SMPTE 29.97 DF facts:
	//   00:00:59;29 -> 00:01:00;02 (00,01 skipped)
	//   00:09:59;29 -> 00:10:00;00 (no skip at 10th minute)
	//   00:00:00;00 == frame 0; 00:01:00;02 == frame 1800
	cases := []struct {
		tc    string
		frame int64
		ns    int64
	}{
		{"00:00:00;00", 0, 0},
		{"00:00:59;29", 1799, 1799 * 1_000_000_000 * 1001 / 30000},
		{"00:01:00;02", 1800, 1800 * 1_000_000_000 * 1001 / 30000},
		{"00:09:59;29", 17981, 17981 * 1_000_000_000 * 1001 / 30000},
		{"00:10:00;00", 17982, 17982 * 1_000_000_000 * 1001 / 30000},
	}
	for _, c := range cases {
		tc, _, err := ParseTimecode(c.tc, Rate2997)
		if err != nil {
			t.Errorf("ParseTimecode(%q): %v", c.tc, err)
			continue
		}
		frames, err := tc.totalFrames()
		if err != nil {
			t.Errorf("totalFrames(%q): %v", c.tc, err)
			continue
		}
		if frames != c.frame {
			t.Errorf("%q totalFrames = %d, want %d", c.tc, frames, c.frame)
		}
		if tc.Nanos() != c.ns {
			t.Errorf("%q nanos = %d, want %d", c.tc, tc.Nanos(), c.ns)
		}
	}
}

func TestTimecodeDropFrameSkipsIllegalLabel(t *testing.T) {
	// 00:01:00;00 and ;01 are not valid DF labels.
	for _, s := range []string{"00:01:00;00", "00:01:00;01", "00:01:00;29"} {
		_, _, err := ParseTimecode(s, Rate2997)
		if s == "00:01:00;29" {
			// ;29 is legal: only 00/01 are skipped.
			if err != nil {
				t.Errorf("ParseTimecode(%q) unexpected error: %v", s, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("ParseTimecode(%q) = nil error, want rejection", s)
		}
	}
}

func TestTimecodeFromFramesRoundTrip(t *testing.T) {
	rates := []FrameRate{Rate24, Rate25, Rate30, Rate2997}
	for _, r := range rates {
		for _, frame := range []int64{0, 1, 24, 1799, 1800, 18000, 17999, 86399, 86400, 259200} {
			tc, err := TimecodeFromFrames(frame, r)
			if err != nil {
				t.Fatalf("%s frame %d: %v", r.Label(), frame, err)
			}
			parsed, _, err := ParseTimecode(tc.String(), r)
			if err != nil {
				t.Fatalf("%s frame %d rendered %q failed to reparse: %v", r.Label(), frame, tc.String(), err)
			}
			got, err := parsed.totalFrames()
			if err != nil {
				t.Fatal(err)
			}
			if got != frame {
				t.Errorf("%s frame %d -> %q -> frame %d", r.Label(), frame, tc.String(), got)
			}
		}
	}
}

// TestCrossDayRoundTripExact verifies that multi-day labels survive a round
// trip exactly, including drop-frame rates where a label-day is shorter than
// 24*3600*nominal frames. This guards a subtle bug where a "D+1/" offset was
// added in nominal rather than label-day frames.
func TestCrossDayRoundTripExact(t *testing.T) {
	const dropDay = int64(24*60*60*30 - 2*(24*60-24*6)) // 2,589,408 frames
	cases := []struct {
		rate  FrameRate
		frame int64
	}{
		{Rate2997, 0}, {Rate2997, 1},
		{Rate2997, dropDay - 1}, {Rate2997, dropDay}, {Rate2997, dropDay + 1},
		{Rate2997, 2 * dropDay}, {Rate2997, 2*dropDay + 20},
		{Rate25, 0}, {Rate25, 86400}, {Rate25, 2 * 86400},
	}
	for _, c := range cases {
		tc, err := TimecodeFromFrames(c.frame, c.rate)
		if err != nil {
			t.Fatalf("%s frame %d: %v", c.rate.Label(), c.frame, err)
		}
		parsed, _, err := ParseTimecode(tc.String(), c.rate)
		if err != nil {
			t.Fatalf("%s frame %d rendered %q failed to reparse: %v", c.rate.Label(), c.frame, tc.String(), err)
		}
		got, err := parsed.totalFrames()
		if err != nil {
			t.Fatal(err)
		}
		if got != c.frame {
			t.Errorf("%s frame %d -> %q -> frame %d", c.rate.Label(), c.frame, tc.String(), got)
		}
	}
}

func TestTimecodeNegativeAndWrap(t *testing.T) {
	// Negative offset is preserved and maps to negative nanos.
	tc, _, err := ParseTimecode("-00:00:01:00", Rate25)
	if err != nil {
		t.Fatal(err)
	}
	if tc.Nanos() != -NanosPerSecond {
		t.Errorf("negative tc nanos = %d, want %d", tc.Nanos(), -NanosPerSecond)
	}
	if tc.String() != "-00:00:01:00" {
		t.Errorf("negative tc string = %q", tc.String())
	}
	// Day wrap: 23:59:59:24 @25 rolls to D+1/00:00:00:00 via explicit prefix.
	tc, _, err = ParseTimecodeWithDay("D+1/00:00:00:00", Rate25, 0)
	if err != nil {
		t.Fatal(err)
	}
	if tc.DayOffset != 1 {
		t.Fatalf("day offset = %d, want 1", tc.DayOffset)
	}
	if tc.Nanos() != 86400*NanosPerSecond {
		t.Errorf("day-wrap nanos = %d, want %d", tc.Nanos(), 86400*NanosPerSecond)
	}
	// Late-crossing midnight: 23:59:59:24 is one frame before midnight.
	tc, _, err = ParseTimecode("23:59:59:24", Rate25)
	if err != nil {
		t.Fatal(err)
	}
	if tc.Nanos() != 86400*NanosPerSecond-40_000_000 {
		t.Errorf("23:59:59:24 nanos = %d", tc.Nanos())
	}
}

func TestTimecodeRejectsOutOfRange(t *testing.T) {
	bad := []string{"00:60:00:00", "00:00:60:00", "00:00:00:25", "00:00:00", "aa:00:00:00", ""}
	for _, s := range bad {
		if _, _, err := ParseTimecode(s, Rate25); err == nil {
			t.Errorf("ParseTimecode(%q) accepted invalid timecode", s)
		}
	}
	if _, _, err := ParseTimecode("00:00:00;00", Rate25); err == nil {
		t.Error("drop-frame separator accepted at integer rate")
	}
	if !errors.Is(ErrBadTimecode, ErrBadTimecode) {
		t.Fatal("sentinel not self-consistent")
	}
}

func TestNonIntegralFrameTimeReported(t *testing.T) {
	// 10ms into a 25fps frame (40ms) is not frame aligned.
	_, rem, err := Rate25.NanosToFrame(10_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if rem != 10_000_000 {
		t.Errorf("remainder = %d, want 10000000", rem)
	}
}

func TestOverflowGuards(t *testing.T) {
	if _, err := Rate25.FrameToNanos(1 << 62); err == nil {
		t.Error("expected overflow error for huge frame index")
	}
	// 1<<62 ns is ~146 years; its frame index still fits int64 for 25fps, so
	// it must succeed rather than overflow.
	if frame, _, err := Rate25.NanosToFrame(1 << 62); err != nil || frame <= 0 {
		t.Errorf("NanosToFrame(1<<62) = %d, %v; want a large positive frame", frame, err)
	}
	if _, _, err := Rate25.NanosToFrame(-1); err == nil {
		t.Error("expected error for negative ns")
	}
}
