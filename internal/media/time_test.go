package media

import (
	"errors"
	"math"
	"testing"
)

func TestRationalCompareAndScale(t *testing.T) {
	cases := []struct {
		r, o Rational
		want int
	}{
		{Rational{1, 2}, Rational{2, 4}, 0},
		{Rational{1, 3}, Rational{1, 2}, -1},
		{Rational{3, 2}, Rational{1, 1}, 1},
		{Rational{-1, 4}, Rational{1, 4}, -1},
	}
	for _, c := range cases {
		if got := c.r.Compare(c.o); got != c.want {
			t.Fatalf("%v.Compare(%v)=%d want %d", c.r, c.o, got, c.want)
		}
	}
	if got := (Rational{Num: 1, Den: 48_000}).Scale(48_000); got != 1 {
		t.Fatalf("scale 1 tick = %d, want 1", got)
	}
	if got := (Rational{Num: 1, Den: 3}).Scale(1000); got != 333 {
		t.Fatalf("round scale = %d want 333", got)
	}
	neg := Rational{Num: -1, Den: 3}
	if got := neg.Scale(1000); got != -333 {
		t.Fatalf("negative round scale = %d want -333", got)
	}
}

func TestRationalArithmetic(t *testing.T) {
	a := Rational{Num: 1, Den: 3}
	b := Rational{Num: 1, Den: 6}
	got := a.Add(b)
	if got.Num != 1 || got.Den != 2 {
		t.Fatalf("1/3+1/6 = %v want 1/2", got)
	}
	diff := a.Sub(b)
	if diff.Num != 1 || diff.Den != 6 {
		t.Fatalf("1/3-1/6 = %v want 1/6", diff)
	}
	// 跨分母相加须精确约分。
	c := Rational{Num: 1, Den: 7}
	got = a.Add(c)
	if got.Seconds() != 1.0/3.0+1.0/7.0 {
		t.Fatalf("cross-den add = %v", got)
	}
}

func TestRescaleAndSamples(t *testing.T) {
	r := Rational{Num: 5, Den: 1000} // 5 ms
	got := r.Rescale(TBAudio48k)
	if got.Num != 240 || got.Den != 48_000 {
		t.Fatalf("rescale 5ms = %v want 240/48000", got)
	}
	tt := SamplesToTime(480, 48_000)
	if tt.Millis() != 10 {
		t.Fatalf("480 samples = %d ms want 10", tt.Millis())
	}
	if math.Abs(r.Seconds()-0.005) > 1e-12 {
		t.Fatalf("seconds = %v", r.Seconds())
	}
}

func TestNewRationalRejectsBadDen(t *testing.T) {
	if _, err := NewRational(1, 0); err == nil {
		t.Fatal("零分母必须报错")
	}
	if (Rational{Num: 1}).Valid() {
		t.Fatal("零分母时基非法")
	}
}

func TestMediaErrorModel(t *testing.T) {
	base := errors.New("crc32 mismatch")
	loc := ErrorLocation{StreamID: 7, Epoch: 2, Seq: 42, ArrivalIndex: 3, ByteOffset: 128}
	err := E(KindPacketCorrupt, "packet.validate", loc, "校验失败", base)
	if !IsKind(err, KindPacketCorrupt) {
		t.Fatal("类别判断失败")
	}
	if !errors.Is(err, base) {
		t.Fatal("errors.Is 无法穿透到底层错误")
	}
	wrapped := Wrap(err, "pipeline.run", "处理中断")
	if !IsKind(wrapped, KindPacketCorrupt) {
		t.Fatal("包装后类别丢失")
	}
	var me *MediaError
	if !errors.As(wrapped, &me) || me.Loc.Seq != 42 {
		t.Fatal("包装后位置丢失")
	}
	if KindOf(errors.New("plain")) != KindUnknown {
		t.Fatal("普通错误应为 KindUnknown")
	}
}
