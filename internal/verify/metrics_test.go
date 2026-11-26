package verify

import (
	"math"
	"testing"

	"archivemedia/internal/model"
)

func TestPSNR(t *testing.T) {
	orig := []byte{100, 110, 120, 130}
	// 完全相同 -> 无失真哨兵
	if got := PSNR(orig, append([]byte(nil), orig...)); got != 99.0 {
		t.Fatalf("无失真 PSNR=%f", got)
	}
	// 恒定偏移 10 的恢复帧
	rec := []byte{110, 120, 130, 140}
	got := PSNR(orig, rec)
	want := 10 * math.Log10((255.0*255.0)/100.0) // MSE=100
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("PSNR=%f want %f", got, want)
	}
	// 小偏移（MSE=1）应高于 32dB 门限
	small := []byte{101, 121, 121, 131}
	if PSNR(orig, small) < 32 {
		t.Fatal("MSE=1 的小失真 PSNR 应高于门限")
	}
	// 长度不一致也应能计算而不 panic
	_ = PSNR(orig, []byte{110, 120})
}

func TestRMSDb(t *testing.T) {
	if got := RMSDb(nil); !math.IsInf(got, -1) {
		t.Fatal("空样本应为 -Inf")
	}
	// 满量程 1.0 -> 0 dBFS
	if got := RMSDb([]float64{1, 1}); math.Abs(got) > 1e-9 {
		t.Fatalf("满量程 rms=%f", got)
	}
	// 0.5 -> -6.02 dBFS
	if got := RMSDb([]float64{0.5, 0.5}); math.Abs(got-(-6.0206)) > 0.01 {
		t.Fatalf("半幅 rms=%f", got)
	}
}

func TestEvaluatePassing(t *testing.T) {
	// 1 秒、100000 字节 -> 实际 800kbps；目标 800kbps，偏差 0
	orig := make([]byte, 4096)
	for i := range orig {
		orig[i] = byte(i * 7)
	}
	rec := append([]byte(nil), orig...)
	rec[10]++ // 4096 个样本中仅 1 个差 1，MSE≈0.00024，PSNR 远超 32dB
	metrics := []SegmentMetric{{
		SegmentID: "s1", Bytes: 100_000, Duration: model.Micros(1_000_000),
		TargetBitrate: 800_000, AlignErrorUs: 500,
		OriginalY: orig, RecoveredY: rec, AudioSamples: []float64{0.1, 0.1},
	}}
	rep, err := Evaluate(metrics, 2, 1, "prov-abc", DefaultThresholds())
	if err != nil {
		t.Fatalf("应通过门限: %v", err)
	}
	if rep.TargetBitrateBps != 800_000 || rep.ActualBitrateBps != 800_000 {
		t.Fatalf("码率 target=%d actual=%d", rep.TargetBitrateBps, rep.ActualBitrateBps)
	}
	if math.Abs(rep.BitrateDeltaPct) > 0.01 {
		t.Fatalf("码率偏差=%f", rep.BitrateDeltaPct)
	}
	if rep.AlignErrorMs != 0.5 || rep.AlignErrorMs > 1 {
		t.Fatalf("对齐误差=%fms", rep.AlignErrorMs)
	}
	if rep.RecoveredSegments != 1 || rep.SilenceIntervals != 2 ||
		rep.DuplicateGroups != 1 || rep.ProvenanceSha256 != "prov-abc" {
		t.Fatalf("汇总字段错误: %+v", rep)
	}
	if rep.PsnrDb < 32 {
		t.Fatalf("PSNR=%f 应高于门限", rep.PsnrDb)
	}
}

func TestEvaluateBitrateThresholdFailure(t *testing.T) {
	// 实际码率比目标高 20%，超过 8% 门限
	metrics := []SegmentMetric{{
		SegmentID: "s1", Bytes: 120_000, Duration: model.Micros(1_000_000),
		TargetBitrate: 800_000, // 实际 960kbps
		OriginalY:     []byte{1}, RecoveredY: []byte{1},
	}}
	_, err := Evaluate(metrics, 0, 0, "p", DefaultThresholds())
	if model.KindOf(err) != model.KindThreshold {
		t.Fatalf("码率超门限应为 threshold 错误, got %v", err)
	}
}

func TestEvaluateAlignmentFailure(t *testing.T) {
	metrics := []SegmentMetric{{
		SegmentID: "s1", Bytes: 100_000, Duration: model.Micros(1_000_000),
		TargetBitrate: 800_000, AlignErrorUs: 2_000, // 2ms > 1ms
		OriginalY: []byte{1}, RecoveredY: []byte{1},
	}}
	_, err := Evaluate(metrics, 0, 0, "p", DefaultThresholds())
	if model.KindOf(err) != model.KindThreshold {
		t.Fatalf("对齐超门限应为 threshold, got %v", err)
	}
}

func TestEvaluatePSNRFailure(t *testing.T) {
	orig := make([]byte, 100)
	bad := make([]byte, 100)
	for i := range bad {
		bad[i] = 255 // 巨大失真
	}
	metrics := []SegmentMetric{{
		SegmentID: "s1", Bytes: 100_000, Duration: model.Micros(1_000_000),
		TargetBitrate: 800_000, OriginalY: orig, RecoveredY: bad,
	}}
	_, err := Evaluate(metrics, 0, 0, "p", DefaultThresholds())
	if model.KindOf(err) != model.KindThreshold {
		t.Fatalf("PSNR 超门限应为 threshold, got %v", err)
	}
}
