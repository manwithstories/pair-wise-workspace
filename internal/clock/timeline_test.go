package clock

import (
	"testing"

	"github.com/pion/rtp"
)

func TestUnwrap32Rollover(t *testing.T) {
	// 模拟 90kHz 视频时间戳在 0xFFFFFFFF 附近回绕。
	prev := uint64(0xFFFFFFF0)
	out, wrapped, backward := Unwrap32(prev, uint32(0x00000008))
	if backward {
		t.Fatalf("回绕不应被判为反向跳变")
	}
	if !wrapped {
		t.Fatalf("跨越 2^32 边界应标记 wrapped")
	}
	if want := uint64(0x100000008); out != want {
		t.Fatalf("展开值=%x, 期望 %x", out, want)
	}
}

func TestUnwrap32BackwardJump(t *testing.T) {
	prev := uint64(1_000_000)
	out, _, backward := Unwrap32(prev, uint32(900_000))
	if !backward {
		t.Fatalf("超过半程的反向跳变必须标记 backward")
	}
	if out != 900_000 {
		t.Fatalf("反向跳变展开值=%d, 期望 900000", out)
	}
}

func TestUnwrapRTPReusesWraparoundLogic(t *testing.T) {
	prev := uint64(0xFFFFFFF6)
	pkt := &rtp.Packet{Header: rtp.Header{Timestamp: 0x0000000A}}
	out, wrapped, backward := UnwrapRTP(prev, pkt)
	if !wrapped || backward {
		t.Fatalf("RTP 包回绕判定错误 wrapped=%v backward=%v", wrapped, backward)
	}
	if want := uint64(0x10000000A); out != want {
		t.Fatalf("RTP 展开值=%x, 期望 %x", out, want)
	}
}

func TestRationalMicros(t *testing.T) {
	// 90kHz：3003 滴答对应 33366 微秒（NTSC 一帧）。
	r := Rational{Num: 90000, Den: 1}
	if got := r.Micros(3003); got != 33366 {
		t.Fatalf("Micros(3003)=%d, 期望 33366", got)
	}
}

func TestTimelineRolloverMarksFrame(t *testing.T) {
	tl := NewTimeline(0, Rational{Num: 90000, Den: 1}, 3000)
	tl.Append(0xFFFFFFF0, true)
	m := tl.Append(0x00000008, false)
	if !m.Wrapped || m.Backward {
		t.Fatalf("回绕帧标记错误: %+v", m)
	}
	if m.Ticks64 != 0x100000008 {
		t.Fatalf("展开滴答=%x", m.Ticks64)
	}
	if m.Micros <= 0 {
		t.Fatalf("微秒时间必须为正: %d", m.Micros)
	}
}

func TestTimelineDriftEstimateAndClamp(t *testing.T) {
	tl := NewTimeline(1, Rational{Num: 90000, Den: 1}, 3000)
	// 每帧 3003 滴答 => 约 +1000 PPM，落在限幅边界内。
	for i := 0; i < 100; i++ {
		tl.Append(uint32(i*3003), i%30 == 0)
	}
	if tl.IsVFR() {
		t.Fatalf("恒定含漂移流不应判为 VFR")
	}
	ppm := tl.DriftPPM()
	if ppm <= 990 || ppm > MaxDriftPPM {
		t.Fatalf("漂移 PPM=%v, 期望约 1000 且不超限幅", ppm)
	}
}

func TestTimelineDriftClampUpperBound(t *testing.T) {
	tl := NewTimeline(0, Rational{Num: 90000, Den: 1}, 1000)
	// 每帧 2000 滴答 => +1,000,000 PPM，必须被限幅。
	for i := 0; i < 100; i++ {
		tl.Append(uint32(i*2000), i%30 == 0)
	}
	if got := tl.DriftPPM(); got != MaxDriftPPM {
		t.Fatalf("漂移限幅=%v, 期望 %v", got, MaxDriftPPM)
	}
}

func TestTimelineDetectsRealVFRAndRefusesSmoothing(t *testing.T) {
	tl := NewTimeline(2, Rational{Num: 90000, Den: 1}, 3000)
	steps := []uint32{3000, 3000, 3000, 6000} // 25% 长帧，超过阈值
	var ts uint32
	for i := 0; i < 128; i++ {
		tl.Append(ts, i%16 == 0)
		ts += steps[i%len(steps)]
	}
	if !tl.IsVFR() {
		t.Fatalf("应识别为真实 VFR")
	}
	if tl.DriftPPM() != 0 {
		t.Fatalf("VFR 轨道禁止漂移平滑, got PPM=%v", tl.DriftPPM())
	}
}
