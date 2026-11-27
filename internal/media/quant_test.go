package media

import "testing"

func TestQuantizeRoundTrip(t *testing.T) {
	for _, s := range []float64{0, 0.5, -0.5, 1, -1, 1.5, -1.5, 0.123456, -0.98765} {
		q := Quantize16(s)
		back := Dequantize16(q)
		q2 := Quantize16(back)
		if q != q2 {
			t.Fatalf("s=%v q=%d requant=%d", s, q, q2)
		}
	}
	if Quantize16(2) != 32767 {
		t.Fatal("positive overload not clamped")
	}
	if Quantize16(-2) != -32768 {
		t.Fatal("negative overload not clamped")
	}
	if Quantize16(0.25) != 8192 {
		t.Fatalf("0.25 -> %d", Quantize16(0.25))
	}
}
