package clock

import (
	"math"
	"testing"

	"relay/internal/media"
)

func TestObserveRTPWrapAndReorder(t *testing.T) {
	r := NewRecover(48000)
	// 从接近 32 位边界开始，连续推进跨越回绕点。
	r.ObserveRTP(0xFFFFFFF0)
	ext := r.ObserveRTP(0x00000010)
	// 0xFFFFFFF0 前进 32 个 tick 跨过 2^32 边界：扩展值 = 2^32 + 0x10。
	if ext != int64(0x100000010) {
		t.Fatalf("wrap extension=%#x", ext)
	}
	if r.WrapCount() != 1 {
		t.Fatalf("wrap count=%d", r.WrapCount())
	}
	// 乱序到达一个回绕前的旧时间戳，扩展值不得倒退参考点。
	old := r.ObserveRTP(0xFFFFFFF8)
	if old != int64(0xFFFFFFF8) {
		t.Fatalf("reorder extension=%#x", old)
	}
}

func TestSRDrift150PPM(t *testing.T) {
	const rate = int64(48000)
	r := NewRecover(48000)
	// 锚点 A：RTP=0 对应 NTP=100s。
	r.ObserveRTP(0)
	r.ObserveSR(SRPoint{NTPSeconds: 100, NTPFraction: 0, RTP: 0})

	// 锚点 B：发送端偏快 150ppm，RTP 前进 10s 时墙钟只走 9.9985s。
	wallDeltaNs := media.ScalePPM(10*media.RateNanosec, 150)
	rtpB := uint32(rate * 10)
	r.ObserveRTP(rtpB)
	r.ObserveSR(SRPoint{
		NTPSeconds:  100 + uint32(wallDeltaNs/media.RateNanosec),
		NTPFraction: uint32(int64(uint32(wallDeltaNs%media.RateNanosec)) << 32 / media.RateNanosec),
		RTP:         rtpB,
	})

	got := r.PPM()
	if math.Abs(float64(got-150)) > 25 {
		t.Fatalf("estimated ppm=%d want ~150", got)
	}
	if math.Abs(float64(got)) > media.MaxPPM {
		t.Fatalf("ppm exceeds clamp: %d", got)
	}
}

func TestPPMClampAt200(t *testing.T) {
	r := NewRecover(48000)
	r.ObserveRTP(0)
	// 制造远超 200ppm 的斜率：墙钟增量只有 RTP 标称的一半。
	r.ObserveSR(SRPoint{NTPSeconds: 100, RTP: 0})
	r.ObserveRTP(48000 * 10)
	r.ObserveSR(SRPoint{NTPSeconds: 105, RTP: 48000 * 10})
	if r.PPM() != media.MaxPPM {
		t.Fatalf("ppm=%d, must clamp at +200", r.PPM())
	}
}

func TestToNanosMapping(t *testing.T) {
	r := NewRecover(48000)
	r.ObserveRTP(0)
	r.ObserveSR(SRPoint{NTPSeconds: 1000, NTPFraction: 0, RTP: 0})
	// 48000 tick = 1 秒；无漂移时映射到 NTP 1001s。
	mt := r.ToNanos(48000)
	want := int64(1000-ntpEpochOffsetTest)*media.RateNanosec + media.RateNanosec
	if mt.Ticks != want {
		t.Fatalf("nanos=%d want %d", mt.Ticks, want)
	}
}

const ntpEpochOffsetTest = 2208988800

func TestVFRCalibration(t *testing.T) {
	v := NewVFRTracker()
	// 标记间隔（90k tick）：1500=60fps、1500=60fps、3000=30fps、6000=15fps。
	markers := []uint32{0, 1500, 3000, 6000, 12000}
	var fps []float64
	var inRange []bool
	for _, m := range markers {
		res := v.Observe(m)
		if m != 0 {
			fps = append(fps, res.FPS)
			inRange = append(inRange, res.InRange)
		}
	}
	wantFPS := []float64{60, 60, 30, 15}
	for i := range wantFPS {
		if math.Abs(fps[i]-wantFPS[i]) > 0.01 {
			t.Fatalf("frame %d fps=%v want %v", i, fps, wantFPS)
		}
		if !inRange[i] {
			t.Fatalf("frame %d marked out of range: %v", i, fps)
		}
	}
	// 超出 60fps 的间隔（如 100fps）必须被标为越界。
	fast := v.Observe(12000 + 900) // 900/90000 = 10ms = 100fps
	if fast.InRange || fast.FPS <= VFRMaxFPS*1.01 {
		t.Fatalf("100fps must be out of range: %+v", fast)
	}
	// 纳秒时间轴必须单调。
	if v.Observe(12000+900+3000).Nanos <= fast.Nanos {
		t.Fatal("nanos timeline not monotonic")
	}
}

func TestSRPointNTPNanos(t *testing.T) {
	s := SRPoint{NTPSeconds: 2208988800 + 1, NTPFraction: 0}
	if got := s.NTPNanos(); got != media.RateNanosec {
		t.Fatalf("ntp nanos=%d want 1e9", got)
	}
	// 小数部分：0x80000000 ≈ 0.5 秒。
	half := SRPoint{NTPSeconds: 2208988800, NTPFraction: 0x80000000}
	if got := half.NTPNanos(); got != media.RateNanosec/2 {
		t.Fatalf("fraction nanos=%d want 5e8", got)
	}
}
