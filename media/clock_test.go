package media

import "testing"

func TestConvertExact(t *testing.T) {
	// 1 秒 = 90000 刻度
	if got := Convert(1, Timebase{1, 1}, MediaTimebase); got != 90000 {
		t.Fatalf("1s -> %d", got)
	}
	// RTP 90k -> 毫秒
	if got := Convert(90000, MediaTimebase, Timebase{1, 1000}); got != 1000 {
		t.Fatalf("90000 ticks -> %d ms", got)
	}
	// 恒等换算
	if got := Convert(-42, MediaTimebase, MediaTimebase); got != -42 {
		t.Fatalf("identity -> %d", got)
	}
}

func TestConvertRounding(t *testing.T) {
	// 1/3 秒 -> 30000 刻度（精确）
	if got := Convert(1, Timebase{1, 3}, MediaTimebase); got != 30000 {
		t.Fatalf("1/3s -> %d", got)
	}
	// 1 帧 @30000/1001 -> 3003 刻度（精确）
	if got := FrameRate2997.FrameDuration(); got != 3003 {
		t.Fatalf("29.97 frame -> %d", got)
	}
	// 半数向远离零舍入：1/180000 s = 0.5 tick -> 1；-0.5 -> -1
	if got := Convert(1, Timebase{1, 180000}, MediaTimebase); got != 1 {
		t.Fatalf("half up -> %d", got)
	}
	if got := Convert(-1, Timebase{1, 180000}, MediaTimebase); got != -1 {
		t.Fatalf("half down -> %d", got)
	}
}

func TestConvertNegativeOffset(t *testing.T) {
	// 负偏移：-2 秒
	if got := Convert(-2, Timebase{1, 1}, MediaTimebase); got != -180000 {
		t.Fatalf("-2s -> %d", got)
	}
	if ms := Millis(-180000); ms != -2000 {
		t.Fatalf("Millis(-2s) = %d", ms)
	}
}

func TestConvertNoOverflow(t *testing.T) {
	// 大数值：100 年的毫秒数换算不应溢出
	year100ms := int64(100) * 366 * 86400 * 1000
	got := Convert(Ticks(year100ms), Timebase{1, 1000}, MediaTimebase)
	if got <= 0 {
		t.Fatalf("overflow: %d", got)
	}
	// 往返误差在 1 毫秒以内
	back := Convert(got, MediaTimebase, Timebase{1, 1000})
	if d := int64(back) - year100ms; d < -1 || d > 1 {
		t.Fatalf("roundtrip drift %d", d)
	}
}

func TestFrameRates(t *testing.T) {
	cases := []struct {
		fr   FrameRate
		want Ticks
	}{
		{FrameRate24, 3750},
		{FrameRate25, 3600},
		{FrameRate30, 3000},
		{FrameRate2997, 3003},
	}
	for _, c := range cases {
		if got := c.fr.FrameDuration(); got != c.want {
			t.Errorf("%v frame duration = %d, want %d", c.fr, got, c.want)
		}
	}
	if fps := FrameRate2997.WholeFPS(); fps != 30 {
		t.Fatalf("29.97 whole fps = %d", fps)
	}
	// 29.97 下每帧精确 3003 刻度，107892 帧可整除还原
	if got := FrameRate2997.FramesToTicks(107892); got != 107892*3003 {
		t.Fatalf("107892f@29.97 = %d", got)
	}
}

func TestUnwrapDay(t *testing.T) {
	day := MediaTimebase.Day()
	prev := day - MillisToTicks(500) // 23:59:59.5
	cur := MillisToTicks(500)        // 00:00:00.5（已回绕）
	got := UnwrapDay(prev, cur)
	if got != day+MillisToTicks(500) {
		t.Fatalf("wrap forward = %d", got)
	}
	// 无回绕时保持不变
	if got := UnwrapDay(MillisToTicks(1000), MillisToTicks(2000)); got != MillisToTicks(2000) {
		t.Fatalf("no wrap = %d", got)
	}
	// 向后回绕（乱序到达的前一日事件）
	prev = MillisToTicks(500)
	cur = day - MillisToTicks(500)
	if got := UnwrapDay(prev, cur); got != -MillisToTicks(500) {
		t.Fatalf("wrap backward = %d", got)
	}
}

func TestErrorModel(t *testing.T) {
	e := New(StageParse, "sub", 128, true, "bad cue %d", 7)
	if e.Stage != StageParse || e.Offset != 128 || !e.Recoverable {
		t.Fatalf("fields: %+v", e)
	}
	if !IsRecoverable(e) {
		t.Fatal("should be recoverable")
	}
	plain := As(errTest{})
	if plain.Recoverable || plain.Offset != -1 {
		t.Fatalf("wrapped: %+v", plain)
	}
	if IsRecoverable(nil) {
		// nil 视为可恢复
	} else {
		t.Fatal("nil should be recoverable")
	}
}

type errTest struct{}

func (errTest) Error() string { return "plain" }
