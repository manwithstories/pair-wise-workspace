package media

import (
	"math"
	"testing"
)

func TestRationalNorm(t *testing.T) {
	r := NewRational(3600, 90000)
	if r.Num != 1 || r.Den != 25 {
		t.Fatalf("Norm(3600/90000) = %d/%d, want 1/25", r.Num, r.Den)
	}
	neg := NewRational(3, -6)
	if neg.Num != -1 || neg.Den != 2 {
		t.Fatalf("Norm(3/-6) = %d/%d, want -1/2", neg.Num, neg.Den)
	}
	zero := NewRational(0, 99)
	if zero.Num != 0 || zero.Den != 1 {
		t.Fatalf("Norm(0/99) = %d/%d, want 0/1", zero.Num, zero.Den)
	}
}

func TestTimebaseValidate(t *testing.T) {
	if err := (Timebase{1, 90000}).Validate(); err != nil {
		t.Fatalf("valid timebase rejected: %v", err)
	}
	for _, tb := range []Timebase{{0, 1}, {1, 0}, {-1, 25}} {
		if err := tb.Validate(); err == nil {
			t.Fatalf("invalid timebase %+v accepted", tb)
		}
	}
}

func TestTimebaseConversion(t *testing.T) {
	tb := Timebase{1, 90000}
	if got := tb.ToMillis(90000); got != 1000 {
		t.Fatalf("ToMillis(90000) = %v, want 1000", got)
	}
	if got := tb.FromMillis(1000); got != 90000 {
		t.Fatalf("FromMillis(1000) = %v, want 90000", got)
	}
	// 25fps 帧距 3600 刻度 = 40ms，往返一致
	if got := tb.ToMillis(3600); math.Abs(got-40) > 1e-9 {
		t.Fatalf("ToMillis(3600) = %v, want 40", got)
	}
	r := tb.ToRational(3600)
	if r.Num != 1 || r.Den != 25 {
		t.Fatalf("ToRational(3600) = %d/%d, want 1/25", r.Num, r.Den)
	}
	// 大 PTS（接近 2^33 回绕点）不丢毫秒精度
	big := int64(8589934590)
	if got := tb.FromMillis(tb.ToMillis(big)); got != big {
		t.Fatalf("roundtrip(%d) = %d", big, got)
	}
}
