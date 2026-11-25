package clock

import "testing"

func TestRatReduced(t *testing.T) {
	r := NewRat(2, 4)
	if r.Num != 1 || r.Den != 2 {
		t.Fatalf("NewRat(2,4) = %v, want 1/2", r)
	}
	neg := NewRat(3, -6)
	if neg.Num != -1 || neg.Den != 2 {
		t.Fatalf("NewRat(3,-6) = %v, want -1/2", neg)
	}
}

func TestConvertExact(t *testing.T) {
	// 960 samples @48k == 20ms == 882 samples @44.1k == 1800 ticks @90k.
	a := At(960, Base48k)
	if got := a.Convert(Base44k); got.Ticks != 882 {
		t.Fatalf("960@48k -> %d@44.1k, want 882", got.Ticks)
	}
	if got := a.Convert(Base90k); got.Ticks != 1800 {
		t.Fatalf("960@48k -> %d@90k, want 1800", got.Ticks)
	}
}

func TestAddSubCmpAcrossBases(t *testing.T) {
	a := At(480, Base48k)  // 10ms
	b := At(441, Base44k)  // 10ms
	sum := a.Add(b)        // 20ms in 48k base
	if sum.Ticks != 960 {
		t.Fatalf("10ms+10ms = %d ticks@48k, want 960", sum.Ticks)
	}
	if a.Cmp(b) != 0 {
		t.Fatal("10ms@48k must equal 10ms@44.1k")
	}
	if a.Cmp(At(442, Base44k)) >= 0 {
		t.Fatal("10ms must be < 442 ticks@44.1k")
	}
	if d := sum.Sub(a); d.Ticks != 480 {
		t.Fatalf("20ms-10ms = %d, want 480", d.Ticks)
	}
}

func TestTimebaseRegressionDetection(t *testing.T) {
	// A monotone mux must reject a timestamp that moves backwards.
	prev := At(960, Base48k)
	next := At(480, Base48k)
	if next.Cmp(prev) >= 0 {
		t.Fatal("expected regression to be detected by Cmp")
	}
}

func TestSeconds(t *testing.T) {
	if s := At(48000, Base48k).Seconds(); s != 1.0 {
		t.Fatalf("48000@48k = %fs, want 1.0", s)
	}
}
