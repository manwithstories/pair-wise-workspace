package timeline

import (
	"testing"

	"archivemedia/internal/media"
	"archivemedia/internal/packet"
)

func evtAt(arrival int64) *packet.Event {
	return &packet.Event{
		StreamID: 1, Epoch: 0, Seq: 1,
		Codec: packet.CodecOpus, ClockRate: 48000,
		DurationTicks: 960, ArrivalMicros: arrival,
		ArrivalIndex: 1, Payload: []byte("x"),
	}
}

func TestWatermarkMonotonic(t *testing.T) {
	w := NewWatermark(nil)
	if _, ok := w.Position(); ok {
		t.Fatal("初始水位不应已推进")
	}
	w.Advance(1000)
	w.Advance(500) // 后退请求必须被忽略
	pos, ok := w.Position()
	if !ok || pos != 1000 {
		t.Fatalf("水位 = %d,%v 想保持 1000", pos, ok)
	}
	w.Advance(2000)
	if pos, _ := w.Position(); pos != 2000 {
		t.Fatalf("推进后水位 = %d", pos)
	}
}

func TestLateDetectionAndWindow(t *testing.T) {
	bwe := NewBandwidthEstimator()
	w := NewWatermark(bwe)
	// 喂入稳定 10ms 间隔，窗口应落在最小下界之上。
	for i := int64(0); i < 20; i++ {
		bwe.Observe(i * 10_000)
	}
	win := bwe.LateWindowMicros()
	if win < minLateWindowMicros || win > maxLateWindowMicros {
		t.Fatalf("窗口越界: %d", win)
	}
	w.Advance(1_000_000)
	late := evtAt(1_000_000 + win - 1)
	if !w.IsLate(late) {
		t.Fatal("水位后事件必须判定为迟到")
	}
	if err := w.GuardLate(late); !media.IsKind(err, media.KindTimeline) {
		t.Fatalf("窗口内迟到应为 timeline 回溯: %v", err)
	}
	far := evtAt(1_000_000 + win + 1)
	if err := w.GuardLate(far); !media.IsKind(err, media.KindUnrecoverable) {
		t.Fatalf("超窗迟到应为不可恢复: %v", err)
	}
	onTime := evtAt(1_000_000)
	if w.IsLate(onTime) {
		t.Fatal("水位边界事件不算迟到")
	}
}

func TestWatermarkReset(t *testing.T) {
	w := NewWatermark(nil)
	w.Advance(5_000_000)
	w.Reset(2_000_000)
	pos, ok := w.Position()
	if !ok || pos != 2_000_000 {
		t.Fatalf("回溯后水位 = %d,%v", pos, ok)
	}
	// 回溯后水位仍恢复单调语义。
	w.Advance(1_000_000)
	if pos, _ := w.Position(); pos != 2_000_000 {
		t.Fatal("回溯后旧值推进必须被忽略")
	}
}

func TestBandwidthWindowBounded(t *testing.T) {
	bwe := NewBandwidthEstimator()
	// 空闲估计器返回下界。
	if got := bwe.LateWindowMicros(); got != minLateWindowMicros {
		t.Fatalf("初始窗口 = %d", got)
	}
	// 极大间隔必须被夹到上界。
	bwe.Observe(0)
	for i := 0; i < 50; i++ {
		bwe.Observe(int64(i+1) * 1_000_000_000)
	}
	if got := bwe.LateWindowMicros(); got != maxLateWindowMicros {
		t.Fatalf("高压窗口 = %d 想等于上界 %d", got, maxLateWindowMicros)
	}
}

func TestBandwidthDeterministic(t *testing.T) {
	run := func() int64 {
		b := NewBandwidthEstimator()
		for i := int64(1); i <= 30; i++ {
			b.Observe(i * 12_345)
		}
		return b.LateWindowMicros()
	}
	if run() != run() {
		t.Fatal("带宽估计不确定")
	}
}
