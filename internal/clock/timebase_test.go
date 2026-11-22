package clock

import (
	"errors"
	"math"
	"testing"
)

func TestNewNormalizesAndRejects(t *testing.T) {
	r, err := New(6, 9)
	if err != nil || r != (Rational{Num: 2, Den: 3}) {
		t.Fatalf("New(6,9) = %+v, %v", r, err)
	}
	r2, err := New(-6, -9)
	if err != nil || !r2.Equal(r) {
		t.Fatalf("negative denom not normalized: %+v", r2)
	}
	r3, err := New(6, -9)
	if err != nil || r3 != (Rational{Num: -2, Den: 3}) {
		t.Fatalf("negative denom sign not flipped: %+v", r3)
	}
	if _, err := New(1, 0); !errors.Is(err, ErrZeroDenominator) {
		t.Fatalf("zero denominator not rejected: %v", err)
	}
	if _, err := New(math.MinInt64, -1); !errors.Is(err, ErrOverflow) {
		t.Fatalf("overflow not detected: %v", err)
	}
	if z, err := New(0, 7); err != nil || z != (Rational{0, 1}) {
		t.Fatalf("zero numerator not canonical: %+v, %v", z, err)
	}
}

func TestScaleRoundingAndOverflow(t *testing.T) {
	// 1001/1000 类非整数比例，验证半远离零取整。
	r := MustNew(3, 2)
	cases := map[int64]int64{0: 0, 1: 2, 2: 3, 3: 5, -3: -5, -1: -2, 4: 6}
	for in, want := range cases {
		got, err := r.Scale(in)
		if err != nil || got != want {
			t.Fatalf("Scale(%d) = %d, %v, want %d", in, got, err, want)
		}
	}
	bad := Rational{Num: math.MaxInt64, Den: 1}
	if _, err := bad.Scale(2); !errors.Is(err, ErrOverflow) {
		t.Fatalf("Scale overflow not detected: %v", err)
	}
	var zero Rational
	if _, err := zero.Scale(10); !errors.Is(err, ErrZeroDenominator) {
		t.Fatalf("zero rational Scale not rejected: %v", err)
	}
}

func TestConvertCrossTimebaseWithinOneUnit(t *testing.T) {
	// 视频 30000/1001 与毫秒时基 1000/1 互转，往返误差不超过 1 个 tick。
	from := MustNew(30000, 1001)
	to := MustNew(1000, 1)
	for _, v := range []int64{0, 1, 33, 1001, 30000, 1234567} {
		ms, err := Convert(v, from, to)
		if err != nil {
			t.Fatalf("Convert(%d) error: %v", v, err)
		}
		back, err := Convert(ms, to, from)
		if err != nil {
			t.Fatalf("Convert back error: %v", err)
		}
		d := back - v
		if d < 0 {
			d = -d
		}
		if d > 1 {
			t.Fatalf("roundtrip drift %d for v=%d (ms=%d back=%d)", d, v, ms, back)
		}
	}
	// 48kHz -> 44.1kHz：常见真实时基，误差同样不超过 1 样本。
	audio := MustNew(48000, 1)
	cd := MustNew(44100, 1)
	got, err := Convert(480, audio, cd)
	if err != nil || got != 441 {
		t.Fatalf("480@48k = %d, %v, want 441", got, err)
	}
	if _, err := Convert(1, Rational{}, to); !errors.Is(err, ErrZeroDenominator) {
		t.Fatalf("zero timebase Convert not rejected")
	}
}

func TestRationalMulAndInvert(t *testing.T) {
	r := MustNew(2, 3)
	o := MustNew(9, 4)
	got, err := r.Mul(o)
	if err != nil || got != (Rational{3, 2}) {
		t.Fatalf("Mul = %+v, %v", got, err)
	}
	inv, err := r.Invert()
	if err != nil || inv != (Rational{3, 2}) {
		t.Fatalf("Invert = %+v, %v", inv, err)
	}
	if _, err := (Rational{}).Invert(); !errors.Is(err, ErrZeroDenominator) {
		t.Fatalf("Invert zero not rejected: %v", err)
	}
}

func TestUnwrapRTP(t *testing.T) {
	// 正向回绕。
	prev := int64(0xFFFFFFF0)
	got := UnwrapRTP(prev, 0x00000010)
	if want := prev + 0x20; got != want {
		t.Fatalf("wrap forward = %d, want %d", got, want)
	}
	// 负偏移：重排/负时间戳场景，得到严格小于 prev 的展开值。
	back := UnwrapRTP(1000, 900)
	if back != 900 {
		t.Fatalf("negative delta = %d, want 900", back)
	}
	// 从 0 开始展开，32 位边界附近保持单调。
	seq := []uint32{0, 100, 0xFFFFFF00, 0xFFFFFF80, 0x40}
	var p int64
	for _, ts := range seq {
		p = UnwrapRTP(p, ts)
	}
	if p <= 0 {
		t.Fatalf("unwrapped sequence not monotonic across wrap: %d", p)
	}
}
