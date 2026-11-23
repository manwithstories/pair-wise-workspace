package clock

import "testing"

func TestFromTimescaleRounding(t *testing.T) {
	// 1024 AAC samples at 44100 Hz -> 2089.79 -> 2090 ticks (round half away).
	if got := FromTimescale(1024, 44100); got != Tick(2090) {
		t.Fatalf("got %d, want 2090", got)
	}
	// Exactly representable values (490 units @44100 == 1000 ticks) round trip
	// losslessly; arbitrary values are inherently quantized.
	if got := ToTimescale(FromTimescale(490, 44100), 44100); got != 490 {
		t.Fatalf("exact round trip got %d, want 490", got)
	}
}

func TestMillisRoundTrip(t *testing.T) {
	for _, ms := range []int64{0, 1, 33, 1000, 12345} {
		if got := ToMillis(FromMillis(ms)); got != ms {
			t.Fatalf("ms %d round trip -> %d", ms, got)
		}
	}
}

func TestSnapSample(t *testing.T) {
	starts := []Tick{0, 3000, 6000, 9000}
	snapped, dev := SnapSample(4000, starts)
	if snapped != 3000 || dev != -1000 {
		t.Fatalf("got snap=%d dev=%d", snapped, dev)
	}
	if s, _ := SnapSample(4501, starts); s != 6000 {
		t.Fatalf("nearest tie-break failed: %d", s)
	}
}

func TestZeroTimescale(t *testing.T) {
	if FromTimescale(100, 0) != 0 || ToTimescale(100, 0) != 0 {
		t.Fatal("zero timescale must yield zero, not panic")
	}
}
