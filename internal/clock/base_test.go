package clock

import "testing"

func TestRationalAndTimeBase(t *testing.T) {
	r, err := NewRational(6, 4)
	if err != nil || r.Num != 3 || r.Den != 2 {
		t.Fatalf("NewRational 约分错误: %+v err=%v", r, err)
	}
	if r.MulInt64(8) != 12 {
		t.Fatalf("MulInt64 期望 12, 得 %d", r.MulInt64(8))
	}
	if _, err := NewRational(1, 0); err == nil {
		t.Fatal("分母为 0 必须报错")
	}

	tb, err := NewTimeBase(48000)
	if err != nil {
		t.Fatalf("NewTimeBase: %v", err)
	}
	fps := MustRational(10, 1)
	if spf := tb.SamplesPerFrame(fps); spf.Num != 4800 || spf.Den != 1 {
		t.Fatalf("每帧采样应为 4800, 得 %v", spf)
	}
	if tb.FrameStart(2, fps) != 9600 || tb.FrameOf(9600, fps) != 2 {
		t.Fatal("帧边界换算错误")
	}
	if tb.FrameOf(-1, fps) != -1 {
		t.Fatalf("负采样帧号应向下取整为 -1")
	}
	if tb.FrameCount(192000, fps) != 40 {
		t.Fatal("4 秒 10fps 应为 40 帧")
	}
	// 非整除场景：44100Hz/7fps 仍可精确换算且帧边界单调
	fps7 := MustRational(7, 1)
	prev := int64(-1)
	for s := int64(0); s < 44100; s += 1000 {
		f := tb.FrameOf(s, fps7)
		if f < prev {
			t.Fatal("FrameOf 非单调")
		}
		prev = f
	}
}

func TestErrorCodeAndBlock(t *testing.T) {
	err := NewError(CodeConflict, 7, "内容冲突")
	if CodeOf(err) != CodeConflict || BlockOf(err) != 7 {
		t.Fatalf("错误码/块号提取失败: %v", err)
	}
	wrapped := WrapError(CodeChecksum, -1, err, "校验失败")
	if CodeOf(wrapped) != CodeChecksum {
		t.Fatal("WrapError 错误码丢失")
	}
	if CodeOf(nil) != "" || BlockOf(nil) != -1 {
		t.Fatal("nil 错误提取应返回零值")
	}
}
