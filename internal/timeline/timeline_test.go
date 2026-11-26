package timeline

import (
	"errors"
	"testing"
)

func TestObserveWraparound(t *testing.T) {
	c := NewClock(RTPClockRate)
	first, err := c.Observe(0xFFFFFFF0)
	if err != nil || first != 0 {
		t.Fatalf("first observe: tick=%d err=%v", first, err)
	}
	// 32 ticks later, which crosses the 2^32 boundary.
	got, err := c.Observe(0x10)
	if err != nil {
		t.Fatalf("wraparound observe: %v", err)
	}
	if want := uint64(0x20); got != want {
		t.Fatalf("wraparound tick = %d, want %d", got, want)
	}
}

func TestObserveMonotonicAndSeconds(t *testing.T) {
	c := NewClock(RTPClockRate)
	_, _ = c.Observe(1000)
	// Adjacent packets are 10 ms (900 ticks) apart; one second is 100 steps.
	tick, err := c.Observe(1000 + 100*900)
	if err != nil {
		t.Fatal(err)
	}
	if s := c.Seconds(tick); s != 1.0 {
		t.Fatalf("seconds = %v, want 1.0", s)
	}
}

func TestObserveClockJump(t *testing.T) {
	c := NewClock(RTPClockRate)
	_, _ = c.Observe(0)
	_, err := c.Observe(uint32(MaxSaneJump + 1))
	var te *Error
	if !errors.As(err, &te) || te.Class != ClassFatal {
		t.Fatalf("expected ClassFatal jump error, got %v", err)
	}
	if c.Jumps() != 1 {
		t.Fatalf("jumps = %d, want 1", c.Jumps())
	}
}

func TestSRMapping(t *testing.T) {
	c := NewClock(RTPClockRate)
	// Build media in legal 10 ms steps. First packet at RTP 1000.
	tick0, _ := c.Observe(1000)
	for i := 1; i <= 300; i++ { // advance 3 seconds
		if _, err := c.Observe(1000 + uint32(i*900)); err != nil {
			t.Fatal(err)
		}
	}
	// SR anchors the current instant (RTP 1000+3 s) at NTP 5<<32.
	srRTP := uint32(1000 + 3*RTPClockRate)
	if err := c.SetSR(SRAnchor{NTPTime: 5 << 32, RTPTime: srRTP}); err != nil {
		t.Fatal(err)
	}
	tickSR, _ := c.Observe(srRTP)
	if ntp, ok := c.ToNTP(tickSR); !ok || ntp != 5<<32 {
		t.Fatalf("anchor ntp = %#x ok=%v, want %#x", ntp, ok, uint64(5<<32))
	}
	// 100 more steps = one second after the anchor -> NTP 6.0 exactly.
	tickAfter, _ := c.Observe(srRTP + 100*900)
	if ntp, _ := c.ToNTP(tickAfter); ntp != 6<<32 {
		t.Fatalf("ntp after 1s = %#x, want %#x", ntp, uint64(6<<32))
	}
	// tick0 is 3 s before the anchor: NTP 5 - 3 = 2.0, a valid positive time.
	if ntp, ok := c.ToNTP(tick0); !ok || ntp != 2<<32 {
		t.Fatalf("pre-anchor ntp = %#x ok=%v, want %#x", ntp, ok, uint64(2<<32))
	}
}

func TestSRBeforeEpochRejected(t *testing.T) {
	c := NewClock(RTPClockRate)
	_, _ = c.Observe(0)
	// Anchor at NTP 1s for RTP t=2s; the observed base (tick 0) is then 2 s
	// before the anchor, mapping to NTP -1 which cannot be represented.
	if err := c.SetSR(SRAnchor{NTPTime: 1 << 32, RTPTime: 2 * RTPClockRate}); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.ToNTP(0); ok {
		t.Fatal("expected rejection of NTP conversion before the epoch")
	}
}

func TestErrorClassAndIntegrity(t *testing.T) {
	e := NewError(ClassGap, "rtp.reorder", "missing seq 42", nil)
	if e.Class != ClassGap || e.Error() == "" {
		t.Fatalf("bad error construction: %v", e)
	}
	ie := IntegrityError("cache.verify", "sha mismatch", errors.New("want abc"))
	if !errors.Is(ie, ErrIntegrity) {
		t.Fatal("integrity error must unwrap to ErrIntegrity")
	}
}
