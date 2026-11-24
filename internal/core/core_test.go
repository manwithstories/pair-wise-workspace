package core

import (
	"errors"
	"testing"
	"time"
)

func TestTimestampConversion(t *testing.T) {
	if got := Time(48000).Milliseconds(); got != 1000 {
		t.Fatalf("milliseconds = %.3f, want 1000", got)
	}
	if got := Time(48000).Duration(); got != time.Second {
		t.Fatalf("duration = %v, want 1s", got)
	}
}

func TestRTPExtensionAndWraparound(t *testing.T) {
	c := NewClock(1, 48000)
	values := []uint32{0xfffffff0, 0xfffffff8, 0x00000002, 0x0000000a}
	want := []int64{0xfffffff0, 0xfffffff8, 0x100000002, 0x10000000a}
	for i, v := range values {
		if got := c.ExtendTimestamp(v); got != want[i] {
			t.Fatalf("ExtendTimestamp(%x) = %d, want %d", v, got, want[i])
		}
	}
}

func TestClockMapWithoutSenderReport(t *testing.T) {
	c := NewClock(7, 44100)
	first := c.Map(1000)
	second := c.Map(1000 + 44100)
	if first != 0 {
		t.Fatalf("first map = %d, want origin zero", first)
	}
	if second != 48000 {
		t.Fatalf("one 44.1kHz second mapped to %d samples, want 48000", second)
	}
}

func TestSenderReportCorrectsDrift(t *testing.T) {
	c := NewClock(9, 48000)
	// Two SRs one NTP second apart. RTP advances 48120 instead of 48000:
	// +2500 ppm.
	c.ObserveSenderReport(100, 1000, 0)
	c.ObserveSenderReport(100+48120, 1001, 0)
	if got := c.DriftPPM(); abs(got-2500) > 1 {
		t.Fatalf("drift = %.2f ppm, want about 2500", got)
	}
	if !c.HasSenderReport() {
		t.Fatal("clock should record SR use")
	}
}

func TestErrorExitCodesAndOffset(t *testing.T) {
	err := At(KindParse, "parse", 123, errors.New("bad extension"))
	if KindOf(err) != KindParse {
		t.Fatal("kind not preserved")
	}
	if code := ExitCode(err); code != 1 {
		t.Fatalf("parse exit = %d, want 1 unless strict maps elsewhere", code)
	}
	if got := err.Error(); !contains(got, "at_byte=123") {
		t.Fatalf("error %q missing offset", got)
	}
	if ExitCode(New(KindStrict, "strict", nil)) != 2 {
		t.Fatal("strict must exit 2")
	}
	if ExitCode(New(KindInterrupted, "crash", nil)) != 75 {
		t.Fatal("interrupt must exit 75")
	}
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
