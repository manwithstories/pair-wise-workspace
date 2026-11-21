package clock

import (
	"math"
	"testing"
	"time"
)

func TestFromTimescaleRejectsZero(t *testing.T) {
	if _, err := FromTimescale(0); err == nil {
		t.Fatal("zero timescale accepted")
	}
	if _, err := New(1, 0); err == nil {
		t.Fatal("zero denominator accepted")
	}
}

func TestScaleIsExact(t *testing.T) {
	// 30000 Hz timestamps into a 90000 Hz RTP clock: exact 3x.
	for _, v := range []uint64{0, 1, 1000, 30000, 1 << 30} {
		got, err := Scale(v, 30000, 90000)
		if err != nil {
			t.Fatal(err)
		}
		if got != v*3 {
			t.Fatalf("Scale(%d)=%d want %d", v, got, v*3)
		}
	}
	// 44100 -> 48000 must never accumulate float drift: verify against an
	// exact integer reference at many points.
	const from, to = 44100, 48000
	for v := uint64(0); v < 100000; v += 997 {
		want := v * to / from
		got, err := Scale(v, from, to)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("Scale(%d)=%d want %d", v, got, want)
		}
	}
}

func TestScaleOverflow(t *testing.T) {
	if _, err := Scale(math.MaxUint64, 1, math.MaxUint32); err == nil {
		t.Fatal("expected overflow")
	}
	if _, err := Add(math.MaxUint64, 1); err == nil {
		t.Fatal("expected addition overflow")
	}
}

func TestMonotonic(t *testing.T) {
	var m Monotonic
	for _, v := range []uint64{0, 0, 10, 10, 11} {
		if err := m.Observe(v); err != nil {
			t.Fatalf("Observe(%d): %v", v, err)
		}
	}
	if err := m.Observe(9); err == nil {
		t.Fatal("non-monotonic timestamp accepted")
	}
}

func TestNanosAndMicros(t *testing.T) {
	if got, _ := Nanos(1, 1); got != 1e9 {
		t.Fatalf("Nanos(1,1)=%d", got)
	}
	if got, _ := Micros(30000, 30000); got != 1e6 {
		t.Fatalf("Micros=%d", got)
	}
}

func TestRTPTimestampAndExtension(t *testing.T) {
	epoch := time.Unix(1700000000, 0).UTC()
	pkt, err := Packet(90000, 90000, RTPVideoClockRate, 7, 0xDEADBEEF, 96, epoch, []byte{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	if pkt.Timestamp != 90000 {
		t.Fatalf("rtp timestamp=%d", pkt.Timestamp)
	}
	got, ok := CaptureTime(pkt)
	if !ok {
		t.Fatal("missing capture extension")
	}
	if !got.Equal(epoch.Add(time.Second)) {
		t.Fatalf("capture time=%v", got)
	}
	if _, err := RTPTimestamp(1, 0, 90000); err == nil {
		t.Fatal("zero timescale accepted")
	}
}
