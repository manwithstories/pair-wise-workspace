package main

import (
	"errors"
	"math"
	"testing"
)

// synthPairs 按 a = αv + β 合成 n 对锚点（纳秒），跨度 spanSec 秒。
func synthPairs(n int, spanSec float64, alpha, betaNS float64) []Pair {
	pairs := make([]Pair, n)
	for i := 0; i < n; i++ {
		v := spanSec * 1e9 * float64(i) / float64(n-1)
		a := alpha*v + betaNS
		pairs[i] = Pair{
			SyncID: "s", LineV: int64(i + 1), LineA: int64(i + 1),
			Video: int64(math.Round(v)), Audio: int64(math.Round(a)),
		}
	}
	return pairs
}

func TestFitRecoversTruth(t *testing.T) {
	const alpha = 1.00024 // 240ppm
	const beta = 80_000_000.0
	pairs := synthPairs(40, 120, alpha, beta)
	f, err := FitSegment(0, pairs)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(f.PPM-240) > 1.0 {
		t.Fatalf("ppm=%v want ~240", f.PPM)
	}
	betaGot, _ := ratToInt64Even(f.Beta)
	if math.Abs(float64(betaGot)-beta) > 500_000 {
		t.Fatalf("beta=%d want ~%d", betaGot, int64(beta))
	}
	if f.Inliers != len(pairs) {
		t.Fatalf("clean data should keep all inliers: %d/%d", f.Inliers, len(pairs))
	}
}

func TestFitRejectsAnchorsBeyond2ms(t *testing.T) {
	pairs := synthPairs(40, 120, 1.00024, 80_000_000)
	// 注入 10% 大幅异常（10ms），必须被 2ms 阈值剔除但拟合仍成功。
	for i := 4; i < len(pairs); i += 10 {
		pairs[i].Audio += 10_000_000
	}
	f, err := FitSegment(0, pairs)
	if err != nil {
		t.Fatal(err)
	}
	if f.Inliers >= len(pairs) {
		t.Fatal("anomalous anchors must be rejected")
	}
	if math.Abs(f.PPM-240) > 5 {
		t.Fatalf("ppm after rejection=%v want ~240", f.PPM)
	}
	if f.ResP95NS > 1_000_000 {
		t.Fatalf("p95 residual too large: %d", f.ResP95NS)
	}
}

func TestFitTooFewPairs(t *testing.T) {
	_, err := FitSegment(0, synthPairs(5, 120, 1.0, 0))
	if !errors.Is(err, ErrUnreliable) {
		t.Fatalf("want ErrUnreliable, got %v", err)
	}
}

func TestFitSpanTooShort(t *testing.T) {
	// 10 对但跨度仅 10 秒 < 60 秒门槛。
	_, err := FitSegment(0, synthPairs(10, 10, 1.0, 0))
	if !errors.Is(err, ErrUnreliable) {
		t.Fatalf("want ErrUnreliable, got %v", err)
	}
}

func TestFitInliersBelowMinimum(t *testing.T) {
	// 仅保留 4 个干净锚点（< 6 对门槛），其余施加无规律的大幅扰动，
	// 使任何直线都无法把它们留在 2ms 残差带内。
	pairs := synthPairs(20, 120, 1.0, 0)
	rng := newLCG(42)
	for i := 4; i < len(pairs); i++ {
		mag := 8_000_000 + rng.float()*30_000_000
		if rng.float() < 0.5 {
			mag = -mag
		}
		pairs[i].Audio += int64(mag)
	}
	_, err := FitSegment(0, pairs)
	if !errors.Is(err, ErrUnreliable) {
		t.Fatalf("want ErrUnreliable when inliers < 6, got %v", err)
	}
}

func TestFitQuantizedIdenticalTimestamps(t *testing.T) {
	// 细粒度（1ms）量化会让相邻锚点出现相同视频读数：Theil-Sen 必须
	// 跳过 dx=0 点对而不是产生 Inf/NaN，拟合仍需成立。
	pairs := synthPairs(30, 120, 1.0001, 1_000_000)
	for i := range pairs {
		pairs[i].Video = pairs[i].Video - (pairs[i].Video % 1_000_000)
	}
	f, err := FitSegment(0, pairs)
	if err != nil {
		t.Fatalf("quantized duplicate timestamps must not break fit: %v", err)
	}
	if f.Inliers < 6 {
		t.Fatalf("want >=6 inliers, got %d", f.Inliers)
	}
	if math.Abs(f.PPM-100) > 5 {
		t.Fatalf("ppm under quantization=%v want ~100", f.PPM)
	}
}
