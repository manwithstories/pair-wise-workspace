package clock

import "testing"

func TestNewNormalizes(t *testing.T) {
	if got := New(30000, 1001); got.Num != 30000 || got.Den != 1001 {
		t.Fatalf("got %v", got)
	}
	if got := New(60, 2); !got.Equals(New(30, 1)) {
		t.Fatalf("expected normalization, got %v", got)
	}
}

func TestNewRejects(t *testing.T) {
	for _, tc := range [][2]int64{{0, 1}, {1, 0}, {-1, 1}, {1, -2}} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatalf("expected panic for %v", tc)
				}
			}()
			New(tc[0], tc[1])
		}()
	}
}

func TestRescale(t *testing.T) {
	cases := []struct {
		ticks    int64
		src, dst Rational
		want     int64
	}{
		{90, RTPClock(), Millis(), 1},          // 90kHz -> ms
		{90000, RTPClock(), Millis(), 1000},    // one second
		{30, New(30000, 1001), Millis(), 1001},
		{1000, Millis(), RTPClock(), 90000},
		{1, Millis(), RTPClock(), 90},
	}
	for _, c := range cases {
		if got := Rescale(c.ticks, c.src, c.dst); got != c.want {
			t.Errorf("Rescale(%d, %v, %v) = %d, want %d", c.ticks, c.src, c.dst, got, c.want)
		}
	}
}

func TestRescaleRoundTrip(t *testing.T) {
	// A 90kHz tick is ~11 microseconds, so sub-millisecond values legitimately
	// collapse to 0ms; the round-trip error must stay within one source ms.
	for _, ticks := range []int64{0, 90, 4096, 1 << 40} {
		ms := Rescale(ticks, RTPClock(), Millis())
		back := Rescale(ms, Millis(), RTPClock())
		d := back - ticks
		if d < 0 {
			d = -d
		}
		if d > 90 {
			t.Fatalf("round trip drift at %d: back=%d", ticks, back)
		}
	}
}

func TestUnwrap32(t *testing.T) {
	prev := int64(0xFFFFFFFD)
	seq := []uint32{0xFFFFFFFE, 0xFFFFFFFF, 0, 1, 0}
	want := []int64{0xFFFFFFFE, 0xFFFFFFFF, 1 << 32, 1<<32 + 1, 1 << 32}
	for i, cur := range seq {
		prev = Unwrap32(prev, cur)
		if prev != want[i] {
			t.Fatalf("step %d: got %d want %d", i, prev, want[i])
		}
	}
}

func TestUnwrap32SmallBackwards(t *testing.T) {
	// A small backwards move is jitter, not a wraparound: it must not add 2^32.
	got := Unwrap32(1000, 990)
	if got != 990 {
		t.Fatalf("small backwards step unwrapped to %d", got)
	}
	if Unwrap32(5, 5) != 5 {
		t.Fatal("duplicate counters must stay equal")
	}
}

func TestUnwrap16(t *testing.T) {
	prev := int64(0xFFFE)
	prev = Unwrap16(prev, 0xFFFF)
	if prev != 0xFFFF {
		t.Fatalf("got %d", prev)
	}
	prev = Unwrap16(prev, 0)
	if prev != 1<<16 {
		t.Fatalf("wrap: got %d want %d", prev, 1<<16)
	}
	prev = Unwrap16(prev, 0) // duplicate, not a backwards wrap
	if prev != 1<<16 {
		t.Fatalf("duplicate: got %d", prev)
	}
}
