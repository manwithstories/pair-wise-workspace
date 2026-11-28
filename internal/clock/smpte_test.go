package clock

import (
	"testing"

	"presspack/internal/core"
)

func mustFPS(t *testing.T, s string) FPS {
	t.Helper()
	f, err := ParseFPS(s)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestParseFPS(t *testing.T) {
	for _, s := range []string{"24", "25", "30", "30000/1001", "29.97"} {
		if _, err := ParseFPS(s); err != nil {
			t.Errorf("ParseFPS(%q): %v", s, err)
		}
	}
	for _, s := range []string{"23.976", "60", ""} {
		if _, err := ParseFPS(s); err == nil {
			t.Errorf("ParseFPS(%q) should fail", s)
		}
	}
}

func TestFrameDurationExact(t *testing.T) {
	cases := map[string]int64{"24": 3750, "25": 3600, "30": 3000, "30000/1001": 3003}
	for s, want := range cases {
		if got := mustFPS(t, s).FrameDuration().Ticks; got != want {
			t.Errorf("fps %s frame = %d ticks, want %d", s, got, want)
		}
	}
}

func TestValidateRejectsIllegalFrames(t *testing.T) {
	f25 := mustFPS(t, "25")
	tc, _ := Parse("01:00:00:25")
	if err := tc.Validate(f25); err == nil {
		t.Error("frame 25 at 25fps must be rejected")
	}
	tc, _ = Parse("01:00:00:24")
	if err := tc.Validate(f25); err != nil {
		t.Errorf("frame 24 at 25fps: %v", err)
	}
	tc, _ = Parse("24:00:00:00")
	if err := tc.Validate(f25); err == nil {
		t.Error("hour 24 must be rejected")
	}
}

func TestDropFrameLabels(t *testing.T) {
	f := mustFPS(t, "30000/1001")
	// Dropped labels: 00 and 01 at minute boundaries (except %10).
	tc, _ := Parse("00:01:00;01")
	if err := tc.Validate(f); err == nil {
		t.Error("00:01:00;01 must not exist in DF")
	}
	tc, _ = Parse("00:10:00;01")
	if err := tc.Validate(f); err != nil {
		t.Errorf("00:10:00;01 must exist: %v", err)
	}
	// DF only at 29.97.
	tc, _ = Parse("00:00:00;00")
	if err := tc.Validate(mustFPS(t, "30")); err == nil {
		t.Error("drop-frame at 30fps must be rejected")
	}
}

func TestDropFrameCompensation(t *testing.T) {
	f := mustFPS(t, "30000/1001")
	// At 00:10:00;00 exactly 10 real minutes have elapsed:
	// 18000 labels minus 18 dropped = 17982 frames = 600.0 s at 29.97.
	tc, _ := Parse("00:10:00;00")
	if fn := tc.ToFrameNumber(f); fn != 17982 {
		t.Errorf("00:10:00;00 = frame %d, want 17982", fn)
	}
	tm, err := ToTime(tc, f, 0)
	if err != nil {
		t.Fatal(err)
	}
	// 17982 frames at 30000/1001 = 599.9994 s (DF is ~0.6 ms short of
	// wall-clock 10 minutes by design).
	if tm.Millis() != 599999 {
		t.Errorf("00:10:00;00 = %d ms, want 599999", tm.Millis())
	}
	// One DF day = 2589408 frames.
	if got := FramesPerDay(f, true); got != 2589408 {
		t.Errorf("DF day = %d frames, want 2589408", got)
	}
}

func TestDayWrapResolution(t *testing.T) {
	f := mustFPS(t, "25")
	labels := []string{"23:59:59:20", "23:59:59:24", "00:00:00:00", "00:00:00:05"}
	times, faults := ResolveSequence(labels, f)
	if len(faults.OfKind(core.FaultTimecodeWrap)) != 1 {
		t.Fatalf("want 1 wrap fault, got %v", faults)
	}
	day := FramesPerDay(f, false)
	want := core.FromUnits(day+5, core.Rat(1, 25))
	if times[3] != want {
		t.Errorf("post-wrap time = %v, want %v", times[3], want)
	}
	if times[2].Cmp(times[1]) <= 0 {
		t.Error("wrapped time must still increase monotonically")
	}
}

func TestInvalidLabelInSequence(t *testing.T) {
	f := mustFPS(t, "30")
	_, faults := ResolveSequence([]string{"01:00:00:00", "bad", "01:00:00:31"}, f)
	if n := len(faults.OfKind(core.FaultInvalidTimecode)); n != 2 {
		t.Errorf("want 2 invalid faults, got %d", n)
	}
	if !faults.HasUnrecoverable() {
		t.Error("invalid timecode must be non-recoverable")
	}
}
