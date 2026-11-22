package model

import (
	"errors"
	"math/big"
	"testing"
)

func TestRationalArithmetic(t *testing.T) {
	a := NewRational(1, 3)
	b := NewRational(1, 6)
	if got := a.Add(b); got.Cmp(NewRational(1, 2)) != 0 {
		t.Fatalf("add = %v, want 1/2", got)
	}
	if got := a.Sub(b); got.Cmp(NewRational(1, 6)) != 0 {
		t.Fatalf("sub = %v, want 1/6", got)
	}
	if a.Cmp(b) <= 0 {
		t.Fatal("cmp: 1/3 should exceed 1/6")
	}
	// 负分母在构造时规范化。
	if got := NewRational(1, -4); got.Num != -1 || got.Den != 4 {
		t.Fatalf("normalize negative den = %v", got)
	}
}

func TestRationalNormalizedNoOverflowInTicks(t *testing.T) {
	// 48k 时基下 1.5 秒 = 72000 tick。
	r := NewRational(3, 2)
	if got := r.Ticks(48000); got != 72000 {
		t.Fatalf("ticks = %d, want 72000", got)
	}
	// 来回换算保持有理一致：tfdt 90000 @ 90k => 1s => 90000。
	back := FromTimescale(90000, 90000)
	if back.Ticks(90000) != 90000 {
		t.Fatalf("roundtrip ticks mismatch: %v", back)
	}
}

// TestRationalExactness 用 big.Int 交叉校验，保证有理数比较不发生精度损失。
func TestRationalExactness(t *testing.T) {
	r := NewRational(1234567, 7654321)
	want := new(big.Rat).SetFrac(big.NewInt(1234567), big.NewInt(7654321))
	got := new(big.Rat).SetFrac(big.NewInt(r.Num), big.NewInt(r.Den))
	if got.Cmp(want) != 0 {
		t.Fatalf("rational drifted: got %v want %v", got, want)
	}
	if r.Seconds() <= 0 || r.Milli() <= 0 {
		t.Fatal("display conversion should stay positive")
	}
}

func TestMediaErrorUnwrap(t *testing.T) {
	me := &MediaError{Category: ErrTruncatedBox, Offset: 1234, Message: "bad box"}
	wrapped := errors.New("outer: " + me.Error())
	if _, ok := AsMediaError(me); !ok {
		t.Fatal("direct media error should be recognized")
	}
	if _, ok := AsMediaError(wrapped); ok {
		t.Fatal("string-wrapped error must not be mistaken for structured error")
	}
	if me.Error() == "" || me.Category != ErrTruncatedBox {
		t.Fatal("error string/category mismatch")
	}
}

func TestLayoutsAndRates(t *testing.T) {
	if Layout51.Channels() != 6 || Layout71.Channels() != 8 || LayoutStereo.Channels() != 2 {
		t.Fatal("channel counts mismatch")
	}
	if _, err := ParseLayout("atmos"); err == nil {
		t.Fatal("unknown layout must error")
	} else if me, ok := AsMediaError(err); !ok || me.Category != ErrUnknownLayout {
		t.Fatalf("want ErrUnknownLayout, got %v", err)
	}
	if _, err := ParseLayout("stereo"); err != nil {
		t.Fatal(err)
	}
	if !SupportedRate(44100) || !SupportedRate(48000) || SupportedRate(96000) {
		t.Fatal("rate whitelist mismatch")
	}
}

func TestIntervalDuration(t *testing.T) {
	iv := Interval{Start: NewRational(0, 1), End: NewRational(5, 2)}
	if iv.Duration().Cmp(NewRational(5, 2)) != 0 {
		t.Fatalf("duration = %v", iv.Duration())
	}
}
