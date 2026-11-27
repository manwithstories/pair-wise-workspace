package quality

import (
	"math"
	"testing"
)

func TestHashDeterministicAndChained(t *testing.T) {
	a := HashBytes([]byte("frame-1"))
	b := HashBytes([]byte("frame-1"))
	if a != b || len(a) != 64 {
		t.Fatal("同内容哈希不稳定或长度异常")
	}
	h1 := ChainHash([]string{a, HashBytes([]byte("frame-2"))})
	h2 := ChainHash([]string{a, HashBytes([]byte("frame-2"))})
	if h1 != h2 {
		t.Fatal("链式哈希不确定")
	}
	// 顺序变化必须改变链式摘要
	h3 := ChainHash([]string{HashBytes([]byte("frame-2")), a})
	if h3 == h1 {
		t.Fatal("链式哈希未捕获帧顺序")
	}
}

func TestPSNR(t *testing.T) {
	ref := make([]byte, 256)
	for i := range ref {
		ref[i] = byte(i)
	}
	if v, err := PSNR(ref, ref); err != nil || !math.IsInf(v, 1) {
		t.Fatalf("完全一致应返回 +Inf, 得 %v err=%v", v, err)
	}
	noisy := append([]byte(nil), ref...)
	noisy[0] += 10
	v, err := PSNR(ref, noisy)
	if err != nil || v <= 30 || math.IsInf(v, 0) {
		t.Fatalf("轻微噪声 PSNR 应较高且有限, 得 %v err=%v", v, err)
	}
	if _, err := PSNR(ref, noisy[:10]); err == nil {
		t.Fatal("长度不一致必须报错")
	}
}

func TestBitrateAndDeviation(t *testing.T) {
	// 10fps，40 帧 = 4 秒；共 2,000,000 字节 -> 500000 B/s -> 4Mbps
	bps := Bitrate(40*50000, 40, 10, 1)
	if bps != 4_000_000 {
		t.Fatalf("码率计算错误: %d", bps)
	}
	d := DeviationPct(1_000_000, 1_200_000)
	if math.Abs(d-20) > 1e-9 {
		t.Fatalf("偏差应为 20%%, 得 %f", d)
	}
	if DeviationPct(0, 100) != 0 {
		t.Fatal("目标为 0 时偏差应为 0 而非除零")
	}
}

func TestFinalizeSummary(t *testing.T) {
	frames := []FrameMetric{
		{Index: 0, File: "f0.jpg", SHA256: HashBytes([]byte{0}), Bytes: 1000, PSNR: 40},
		{Index: 1, File: "f1.jpg", SHA256: HashBytes([]byte{1}), Bytes: 1000, PSNR: 36},
	}
	s := Finalize(frames, 4000, 10, 1, 7, 12, 1, 3)
	if s.Frames != 2 || s.ClippedSamples != 7 || s.MissingSamples != 12 ||
		s.MaxErrorSamples != 1 || s.CompensatedFrames != 3 {
		t.Fatalf("汇总音频统计错误: %+v", s)
	}
	// 2 帧 10fps = 0.2s，2000 字节 -> 10000 B/s -> 80000 bit/s
	if s.ActualBPS != 80000 || math.Abs(s.PSNRMean-38) > 1e-9 || s.PSNRMin != 36 {
		t.Fatalf("码率/PSNR 汇总错误: %+v", s)
	}
	if len(s.FrameSHA256) != 64 {
		t.Fatal("链式帧哈希缺失")
	}
}
