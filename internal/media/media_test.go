package media

import "testing"

func TestSampleConversionsRoundTrip(t *testing.T) {
	// 取整毫秒对齐的采样数，保证整数换算可精确往返。
	for _, rate := range []uint32{AudioClockRate, VideoClockRate} {
		samples := int64(rate) // 恰好 1 秒
		ns := SamplesToNS(rate, samples)
		if ns != NSPerSecond {
			t.Fatalf("rate %d: %d samples -> %dns, want 1s", rate, samples, ns)
		}
		if back := NSToSamples(rate, ns); back != samples {
			t.Fatalf("rate %d round trip: %d -> %dns -> %d", rate, samples, ns, back)
		}
	}
	// 负采样数必须保留符号。
	if ns := SamplesToNS(AudioClockRate, -48000); ns != -NSPerSecond {
		t.Fatalf("negative samples: %d", ns)
	}
}

func TestClockDetectsWrapAndBackwards(t *testing.T) {
	c := NewClock(VideoClockRate)
	c.Update(1000)
	samples, _, wrapped, backward := c.Update(1000 + 9000)
	if wrapped || backward || samples != 10000 {
		t.Fatalf("normal step: w=%v b=%v s=%d", wrapped, backward, samples)
	}
	// 正向回绕：从接近 2^32 跳到小值（uint32 差为正且原始值变小）。
	_, _, wrapped, backward = c.Update(500)
	_ = wrapped
	_ = backward
	c2 := NewClock(VideoClockRate)
	c2.Update(1<<32 - 100)
	_, _, wrapped, backward = c2.Update(500)
	if !wrapped || backward || c2.Wraps != 1 {
		t.Fatalf("wrap: w=%v b=%v wraps=%d", wrapped, backward, c2.Wraps)
	}
	// 负向回退：同一回绕周期内时间戳变小。
	c3 := NewClock(AudioClockRate)
	c3.Update(5000)
	_, _, wrapped, backward = c3.Update(4000)
	if wrapped || !backward || c3.Backwards != 1 {
		t.Fatalf("backward: w=%v b=%v", wrapped, backward)
	}
}

func TestExtend16SequenceWrap(t *testing.T) {
	// 65534 -> 65535 -> 0 -> 1，序列号 1 展开为 65537。
	prev := int64(65534)
	if v := Extend16(prev, 1); v != 65537 {
		t.Fatalf("extend16 wrap: %d", v)
	}
	if v := Extend16(100, 90); v != 90 {
		t.Fatalf("extend16 backwards: %d", v)
	}
}

func TestErrorModel(t *testing.T) {
	err := NewError(StageRecover, KindGap, "窗口外缺口").
		WithTrack(TrackVideo).WithOffset(4096)
	if !IsKind(err, KindGap) || err.Offset != 4096 || err.Track != TrackVideo {
		t.Fatalf("error fields lost: %+v", err)
	}
	wrapped := NewError(StageParse, KindIO, "底层失败").Wrap(assertErr("boom"))
	if !IsKind(wrapped, KindIO) {
		t.Fatal("kind not visible through wrap")
	}
	if AsError(assertErr("plain"), StageCLI, KindArg).Kind != KindArg {
		t.Fatal("default kind not applied")
	}
}

type assertErr string

func (e assertErr) Error() string { return string(e) }
