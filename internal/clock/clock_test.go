package clock

import (
	"testing"
)

func TestWrap(t *testing.T) {
	const wp uint64 = 4294967000
	u := NewUnwrapper(7, 90000, wp)
	cases := []struct {
		seq  uint16
		raw  uint32
		want uint64
	}{
		{0, uint32(wp - 200), wp - 200},
		{1, uint32(wp - 100), wp - 100},
		{2, 50, wp + 50}, // crossed the configured wrap point
		{3, 150, wp + 150},
	}
	for _, c := range cases {
		got, err := u.Unwrap(c.seq, c.raw)
		if err != nil {
			t.Fatalf("seq=%d: %v", c.seq, err)
		}
		if got != c.want {
			t.Fatalf("seq=%d raw=%d: got %d want %d", c.seq, c.raw, got, c.want)
		}
	}
	if u.WrapCount() != 1 {
		t.Fatalf("wrap count: got %d want 1", u.WrapCount())
	}
	// Late packet after wrap must replay the pre-wrap extended range.
	late, err := u.Unwrap(1, uint32(wp-100))
	if err != nil {
		t.Fatalf("late: %v", err)
	}
	if late != wp-100 {
		t.Fatalf("late replay: got %d want %d", late, wp-100)
	}
	// Standard 2^32 modulus when no wrap point is configured.
	def := NewUnwrapper(1, 48000, 0)
	if def.WrapPoint != MaxRTP {
		t.Fatalf("default wrap point: got %d", def.WrapPoint)
	}
}

func ntp(sec float64) uint64 {
	s := uint64(sec)
	frac := uint64((sec - float64(s)) * 65536 * 65536)
	return s<<32 | frac
}

func TestDrift(t *testing.T) {
	tl := NewTimeline(0)
	const ssrc uint32 = 42
	const rate uint32 = 48000
	// Device clock runs 180 ppm fast: RTP advances more than wall time.
	ppm := 1.00018
	srs := []SenderReport{
		{ssrc, ntp(1.0), uint32(0)},
		{ssrc, ntp(2.0), uint32(48000 * ppm)},
		{ssrc, ntp(3.0), uint32(2 * 48000 * ppm)},
	}
	for _, sr := range srs {
		if err := tl.ObserveSR(sr, rate); err != nil {
			t.Fatal(err)
		}
	}
	got, ok := tl.DriftPPM(ssrc)
	if !ok {
		t.Fatal("expected calibrated drift")
	}
	if got < 175 || got > 185 {
		t.Fatalf("drift ppm: got %d want ~180", got)
	}
	if tl.Version() < 2 {
		t.Fatalf("version should bump on calibration, got %d", tl.Version())
	}
	// Before the first SR, mapped packets stay explicitly uncertain.
	tl2 := NewTimeline(0)
	st, err := tl2.Map(99, 0, 1000, rate)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Uncertain || st.Calibrated {
		t.Fatal("fresh stream must be marked uncertain, not guessed")
	}
	// A lone SR cannot calibrate: frequency needs two separated reports.
	if err := tl2.ObserveSR(SenderReport{99, ntp(0), 1000}, rate); err != nil {
		t.Fatalf("single SR: %v", err)
	}
	// Two SRs closer than the calibration span keep the interval open.
	if err := tl2.ObserveSR(SenderReport{99, ntp(0.05), 1000 + uint32(0.05*48000)}, rate); err != nil {
		t.Fatal(err)
	}
	if _, ok := tl2.DriftPPM(99); ok {
		t.Fatal("short-span SR pair must not calibrate")
	}
	iv := tl2.UncertainIntervals()
	if len(iv) != 1 || iv[0].SSRC != 99 {
		t.Fatalf("uncertain interval: %+v", iv)
	}
}

func TestMonotonicMapping(t *testing.T) {
	tl := NewTimeline(0)
	const ssrc, rate = uint32(11), uint32(90000)
	var prev int64 = -1
	for i, raw := range []uint32{3000, 6000, 9000} {
		st, err := tl.Map(ssrc, uint16(i), raw, rate)
		if err != nil {
			t.Fatal(err)
		}
		if st.MediaTimeUs < prev {
			t.Fatalf("non-monotonic media time at %d", i)
		}
		prev = st.MediaTimeUs
	}
}
