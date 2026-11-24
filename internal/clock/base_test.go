package clock_test

import (
	"math"
	"testing"

	"github.com/pion/rtcp"

	"mediad/internal/clock"
)

func TestRTPTimestampWrapAndMonotonicOutput(t *testing.T) {
	b, err := clock.New(clock.Rate48000)
	if err != nil {
		t.Fatal(err)
	}
	start := uint32(math.MaxUint32 - 10)
	e0 := b.Unwrap(start)
	e1 := b.Unwrap(start + 20) // wraps through zero
	if e1-e0 != 20 {
		t.Fatalf("wrap delta=%d want 20", e1-e0)
	}

	sr := &rtcp.SenderReport{SSRC: 1, NTPTime: 2208988800 << 32, RTPTime: start}
	m := b.ApplySenderReport(sr, 1234)
	if m.MediaTime != e0 {
		t.Fatalf("SR media=%d want %d", m.MediaTime, e0)
	}
	if got := clock.NTPToUnixNano(sr.NTPTime); got != 0 {
		t.Fatalf("NTP epoch conversion=%d want 0", got)
	}

	pos, err := b.AdvanceOutput(0, 10)
	if err != nil || pos != 10 {
		t.Fatalf("advance pos=%d err=%v", pos, err)
	}
	if _, err := b.AdvanceOutput(5, 1); err == nil {
		t.Fatal("expected non-monotonic expected position error")
	}
	pos, err = b.AdvanceOutput(10, 5)
	if err != nil || pos != 15 {
		t.Fatalf("second advance pos=%d err=%v", pos, err)
	}
}

func TestRejectsUnsupportedRate(t *testing.T) {
	if _, err := clock.New(96000); err == nil {
		t.Fatal("expected unsupported rate error")
	}
}
