package timeline

import (
	"errors"
	"testing"
)

func TestFrameDurationExact(t *testing.T) {
	cases := map[Rate]int64{
		Rate24:   1125000,
		Rate25:   1080000,
		Rate30:   900000,
		RateNTSC: 900900,
	}
	for r, want := range cases {
		got, err := r.FrameDuration()
		if err != nil {
			t.Fatalf("rate %+v: %v", r, err)
		}
		if got != want {
			t.Errorf("rate %+v: got %d want %d", r, got, want)
		}
	}
}

func TestParseTimecodeBasic(t *testing.T) {
	ticks, wraps, err := ParseTimecode("01:00:00:00", Rate25)
	if err != nil {
		t.Fatal(err)
	}
	if wraps != 0 {
		t.Fatalf("unexpected wraps %d", wraps)
	}
	if got := TicksToMillis(ticks); got != 3600000 {
		t.Fatalf("got %d ms want 3600000", got)
	}
}

func TestParseTimecodeNTSC(t *testing.T) {
	// 30000/1001 fps 下 1 秒应为 27000000 刻度（允许整帧取整误差内）
	ticks, _, err := ParseTimecode("00:00:01:00", RateNTSC)
	if err != nil {
		t.Fatal(err)
	}
	// 30 帧 * 900900 刻度 = 27027000，对应 1.001 秒
	if ticks != 30*900900 {
		t.Fatalf("got %d", ticks)
	}
}

func TestParseTimecodeNegative(t *testing.T) {
	ticks, _, err := ParseTimecode("-00:00:02:00", Rate30)
	if err != nil {
		t.Fatal(err)
	}
	if ticks != -2*MasterTimescale {
		t.Fatalf("got %d want %d", ticks, -2*MasterTimescale)
	}
}

func TestParseTimecodeDayWrap(t *testing.T) {
	ticks, wraps, err := ParseTimecode("24:00:00:01", Rate25)
	if err != nil {
		t.Fatal(err)
	}
	if wraps != 1 {
		t.Fatalf("wraps=%d want 1", wraps)
	}
	d, _ := Rate25.FrameDuration()
	if ticks != d {
		t.Fatalf("ticks=%d want %d", ticks, d)
	}
}

func TestParseTimecodeInvalid(t *testing.T) {
	for _, raw := range []string{"1:2:3", "00:61:00:00", "00:00:00:30", "aa:00:00:00", "00:00:00:-1"} {
		_, _, err := ParseTimecode(raw, Rate25)
		var tcErr *TimecodeError
		if !errors.As(err, &tcErr) {
			t.Fatalf("%q: expected TimecodeError, got %v", raw, err)
		}
		if tcErr.Raw != raw {
			t.Fatalf("raw not recorded: %q", tcErr.Raw)
		}
	}
}

func TestPTSToTicksOverflow(t *testing.T) {
	if _, err := PTSToTicks(1<<60, 90000); err == nil {
		t.Fatal("expected overflow error")
	}
	got, err := PTSToTicks(90000, 90000)
	if err != nil || got != MasterTimescale {
		t.Fatalf("got %d err %v", got, err)
	}
	if _, err := PTSToTicks(1, 0); err == nil {
		t.Fatal("expected zero timescale error")
	}
}

func TestFormatRoundTrip(t *testing.T) {
	ticks, _, err := ParseTimecode("12:34:56:10", Rate30)
	if err != nil {
		t.Fatal(err)
	}
	if s := FormatTimecode(ticks, Rate30); s != "12:34:56:10" {
		t.Fatalf("got %q", s)
	}
}
