package clock

import (
	"math"
	"testing"
)

func TestNewRejectsZeroRate(t *testing.T) {
	if _, err := New(0); err == nil {
		t.Fatal("zero rate must be rejected")
	}
	c := MustNew(48000)
	if c.Rate() != 48000 {
		t.Fatalf("rate = %d", c.Rate())
	}
}

func TestUnwrapFirstAndForward(t *testing.T) {
	got := Unwrap(0, false, 1000)
	if got != 1000 {
		t.Fatalf("first unwrap = %d, want 1000", got)
	}
	got = Unwrap(1000, true, 1200)
	if got != 1200 {
		t.Fatalf("forward unwrap = %d, want 1200", got)
	}
}

func TestUnwrapWraparound(t *testing.T) {
	// 从 2^32-100 前进到 50，应展开为 2^32+50。
	prev := MaxUint32 - 100
	got := Unwrap(prev, true, 50)
	want := MaxUint32 + 50
	if got != want {
		t.Fatalf("wrap unwrap = %d, want %d", got, want)
	}
}

func TestUnwrapRollback(t *testing.T) {
	// 时间戳回退 200：落入负半区，应减去 2^32。
	prev := uint64(1000)
	got := Unwrap(prev, true, uint32(800))
	if got != 800 {
		t.Fatalf("rollback unwrap = %d, want 800", got)
	}
	// 跨越 0 的回退：prev=50，ts=2^32-100，结果为 -100。
	got = Unwrap(50, true, uint32(MaxUint32-100))
	var want uint64 = math.MaxUint64 - 99 // uint64(-100) 的回绕表示
	if got != want {
		t.Fatalf("cross-zero rollback = %d, want %d", got, want)
	}
}

func TestUnwrapHalfRangeBoundary(t *testing.T) {
	// RFC 3550：差值恰为 2^31 时落入负半区，按回退处理。
	got := Unwrap(0, true, uint32(MaxUint32/2))
	want := math.MaxUint64 - MaxUint32/2 + 1 // uint64(-2^31)
	if got != want {
		t.Fatalf("half-range boundary = %d, want %d", got, want)
	}
	// 2^31-1 仍是前进。
	got = Unwrap(0, true, uint32(MaxUint32/2-1))
	if got != MaxUint32/2-1 {
		t.Fatalf("just inside forward half = %d", got)
	}
}

func TestMillisRoundTrip(t *testing.T) {
	c := MustNew(48000)
	s := c.MillisToSamples(1000)
	if s != 48000 {
		t.Fatalf("1000ms = %d samples", s)
	}
	if c.ToMillis(48000) != 1000 {
		t.Fatalf("48000 samples -> %d ms", c.ToMillis(48000))
	}
	if c.ToMillis(24) != 1 { // 四舍五入
		t.Fatalf("24 samples -> %d ms, want 1", c.ToMillis(24))
	}
}

func TestMillisDiff(t *testing.T) {
	c := MustNew(1000)
	if d := c.ToMillisDiff(10, 20); d != 10 {
		t.Fatalf("diff = %d", d)
	}
	if d := c.ToMillisDiff(20, 10); d != -10 {
		t.Fatalf("negative diff = %d", d)
	}
}

func TestConvert(t *testing.T) {
	src := MustNew(8000)
	dst := MustNew(48000)
	got, err := dst.Convert(8000, src)
	if err != nil || got != 48000 {
		t.Fatalf("convert = %d, err=%v", got, err)
	}
	got, err = src.Convert(48000, dst)
	if err != nil || got != 8000 {
		t.Fatalf("reverse convert = %d, err=%v", got, err)
	}
}

func TestCheckedArithmeticOverflow(t *testing.T) {
	if _, err := CheckedMul(math.MaxUint64, 2); err == nil {
		t.Fatal("mul overflow not detected")
	}
	if v, err := CheckedMul(123, 3); err != nil || v != 369 {
		t.Fatalf("checked mul = %d, %v", v, err)
	}
	if _, err := CheckedAdd(math.MaxUint64, 1); err == nil {
		t.Fatal("add overflow not detected")
	}
	if v, err := CheckedAdd(123, 1); err != nil || v != 124 {
		t.Fatalf("checked add = %d, %v", v, err)
	}
}
