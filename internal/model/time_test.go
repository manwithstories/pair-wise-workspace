package model

import (
	"errors"
	"testing"
)

func TestRationalAndTicks(t *testing.T) {
	if _, err := NewRational(1, 0); err == nil {
		t.Fatal("den=0 必须报错")
	}
	// 25fps：时基 1/25，第 25 帧刻度 = 1_000_000us
	tb := MustRational(1, 25)
	if got := TicksToTime(25, tb); got.Microseconds() != MicrosPerSecond {
		t.Fatalf("TicksToTime(25,1/25)=%d", got)
	}
	// 48kHz 音频：采样 24000 = 500_000us
	audio := MustRational(1, 48000)
	if got := TicksToTime(24000, audio); got.Microseconds() != 500_000 {
		t.Fatalf("audio ticks=%d", got)
	}
	// 往返：微秒 -> 刻度 -> 微秒，量化误差不超过一个刻度
	for _, tc := range []struct {
		us   int64
		tb   Rational
		tol  int64
	}{
		{33_333, MustRational(1, 30), 33_334},       // 30fps 一帧
		{-123_456, MustRational(1, 48000), 21},      // 负起点音频
		{12_345_678, MustRational(1, 90000), 12},    // 高频时基
	} {
		ticks := TimeToTicks(MediaTime(tc.us), tc.tb)
		back := TicksToTime(ticks, tc.tb).Microseconds()
		if d := abs64(back - tc.us); d > tc.tol {
			t.Fatalf("roundtrip us=%d tb=%v back=%d diff=%d tol=%d", tc.us, tc.tb, back, d, tc.tol)
		}
	}
}

func TestSnapToTicks(t *testing.T) {
	// 40ms 刻度（25fps，时基 1/25 秒；Rational 单位为秒/刻度）
	tb := MustRational(1, 25)
	cases := []struct {
		in       int64
		mode     Rounding
		wantTick int64 // 吸附后的微秒值
	}{
		{50_000, RoundNearest, 40_000},  // 距 40k=10k, 距 80k=30k
		{60_000, RoundNearest, 80_000},  // 等距 -> 远离零点
		{60_000, RoundFloor, 40_000},
		{60_000, RoundCeil, 80_000},
		{40_000, RoundFloor, 40_000},    // 已在刻度上
		{-60_000, RoundNearest, -80_000}, // 负时间等距远离零点
		{-50_000, RoundFloor, -80_000},  // floor 向更小
		{-50_000, RoundCeil, -40_000},
	}
	for _, c := range cases {
		got, err := SnapToTicks(MediaTime(c.in), tb, c.mode)
		if got.Microseconds() != c.wantTick {
			t.Fatalf("Snap(%d,%d)=%d want %d", c.in, c.mode, got, c.wantTick)
		}
		// 误差必须等于 snap-original 且被如实记录
		if err.Microseconds() != got.Microseconds()-c.in {
			t.Fatalf("snap error mismatch: %d", err)
		}
	}
}

func TestDurationAndOrdering(t *testing.T) {
	if DurationOf(MediaTime(100), MediaTime(50)) != 0 {
		t.Fatal("倒置区间 Duration 必须为 0")
	}
	if d := DurationOf(MediaTime(-500), MediaTime(1500)); d.Microseconds() != 2000 {
		t.Fatalf("negative-start duration=%d", d)
	}
	if !MinTime(MediaTime(-9), MediaTime(2)).Equals(MediaTime(-9)) {
		t.Fatal("MinTime")
	}
	if AbsMicros(MediaTime(-7)) != 7 {
		t.Fatal("AbsMicros")
	}
}

func TestMediaErrorClassification(t *testing.T) {
	base := errors.New("eio")
	me := WrapError(KindCorruptMedia, base, "tail packet truncated").
		WithSource("tape_A").WithFirstBad(MediaTime(4_200_000))
	if !errors.Is(me, base) {
		t.Fatal("Unwrap 链断裂")
	}
	if KindOf(me) != KindCorruptMedia || me.SourceID != "tape_A" || me.FirstBadAt.Microseconds() != 4_200_000 {
		t.Fatalf("结构化字段丢失: %+v", me)
	}
	if s := me.Error(); s == "" || !contains(s, "first_bad_at=4200000") {
		t.Fatalf("错误文本缺少坏时间: %q", s)
	}
	if KindOf(errors.New("plain")) != "" {
		t.Fatal("非 MediaError 应返回空分类")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
