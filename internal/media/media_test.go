package media

import (
	"errors"
	"testing"
)

func TestRationalConversionExactForCommonTimescales(t *testing.T) {
	cases := []struct {
		value     int64
		timescale uint32
		want      int64
	}{
		{9000, 90000, 9000},       // 0.1s in 90kHz
		{4800, 48000, 9000},       // 0.1s audio
		{3003, 30000, 9009},       // 100.1ms, rounded to common ticks
		{180000, 90000, 180000},   // 2s
	}
	for _, tc := range cases {
		got := ConvertTicks(tc.value, Rational{Num: 1, Den: int64(tc.timescale)}, CommonTimebase)
		if got != tc.want {
			t.Fatalf("ConvertTicks(%d/%d)=%d want %d", tc.value, tc.timescale, got, tc.want)
		}
	}
	if Rat(2, 4).Cmp(Rat(3, 6)) != 0 {
		t.Fatal("reduced equal rationals should compare equal")
	}
}

func TestUnwrapUint16And32(t *testing.T) {
	last := uint64(65534)
	seq := []uint64{65535, 0, 1}
	for i, raw := range seq {
		got := UnwrapUint(16, last, raw, true)
		want := 65535 + uint64(i)
		if got != want {
			t.Fatalf("seq %d got %d want %d", raw, got, want)
		}
		last = got
	}
	got := UnwrapUint(32, 4_294_967_294, 1, true)
	if got != 4_294_967_297 {
		t.Fatalf("32bit wrap got %d", got)
	}
}

func TestTimelineDiscontinuityAndTFDTFallbackAreReported(t *testing.T) {
	tl := NewTimeline()
	a, _ := tl.Resolve("v", 0, 90000, false)
	b, _ := tl.Resolve("v", 9000, 90000, false)
	if a != 0 || b != 9000 {
		t.Fatalf("normal progression = %d,%d", a, b)
	}
	c, ev := tl.Resolve("v", 100, 90000, true)
	if ev == nil || ev.Kind != ResetDiscontinuity || c <= b {
		t.Fatalf("discontinuity not continuous: c=%d ev=%+v", c, ev)
	}
	d, fallback := tl.Resolve("v", 50, 90000, false)
	if fallback == nil || fallback.Kind != ResetTFDTFallback || d <= c {
		t.Fatalf("tfdt fallback not corrected/reported: d=%d ev=%+v", d, fallback)
	}
	if len(tl.Events) != 2 {
		t.Fatalf("events=%d want 2", len(tl.Events))
	}
}

func TestExitCodeMapping(t *testing.T) {
	if got := ExitCode(nil); got != 0 {
		t.Fatal(got)
	}
	if got := ExitCode(NewError(CodeMalformed, "x", "", "")); got != 2 {
		t.Fatalf("malformed exit=%d want2", got)
	}
	if got := ExitCode(NewError(CodeBlockedDependency, "dag", "", "")); got != 3 {
		t.Fatalf("blocked exit=%d want3", got)
	}
	if got := ExitCode(NewError(CodePathViolation, "store", "", "")); got != 4 {
		t.Fatalf("io exit=%d want4", got)
	}
	wrapped := Wrap(errors.New("disk"), CodeLocalIO, "store", "path", "write failed")
	if _, ok := AsError(wrapped); !ok || ExitCode(wrapped) != 4 {
		t.Fatal("wrapped structured error not detected")
	}
}
