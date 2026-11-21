package clock

import "testing"

func TestFitRateExactLine(t *testing.T) {
	// ns = 2*tick + 7 的精确直线，拟合应还原参数。
	var anchors []Anchor
	for tick := int64(0); tick <= 100; tick += 10 {
		anchors = append(anchors, Anchor{Tick: tick, NS: 2*tick + 7})
	}
	fit, err := FitRate(anchors)
	if err != nil {
		t.Fatal(err)
	}
	if d := fit.Slope - 2.0; d > 1e-9 || d < -1e-9 {
		t.Fatalf("斜率拟合偏差: %v", fit.Slope)
	}
	if d := fit.Intercept - 7.0; d > 1e-9 || d < -1e-9 {
		t.Fatalf("截距拟合偏差: %v", fit.Intercept)
	}
	if fit.MaxResNS != 0 {
		t.Fatalf("精确直线残差应为 0, got %d", fit.MaxResNS)
	}
}

func TestFitRatePPMDrift(t *testing.T) {
	// 90kHz 时钟、180ppm 漂移：tick 每走 1，真实 ns 比标称多 0.00018*ns/tick。
	const nsPerTick = 1_000_000_000 / 90_000
	ppm := 180.0
	slope := nsPerTick * (1 + ppm/1_000_000)
	var anchors []Anchor
	for tick := int64(0); tick <= 900_000; tick += 3000 {
		anchors = append(anchors, Anchor{Tick: tick, NS: int64(slope * float64(tick))})
	}
	fit, err := FitRate(anchors)
	if err != nil {
		t.Fatal(err)
	}
	if d := fit.Slope - slope; d > 1e-6 || d < -1e-6 {
		t.Fatalf("ppm 漂移斜率偏差: got %v want %v", fit.Slope, slope)
	}
}

func TestReconstructorNoErrorAccumulation(t *testing.T) {
	fit := SegmentFit{Slope: 11111.111111111, Intercept: 0}
	r := NewReconstructor(fit, 0, 0)
	// 连续重建 12000 帧，与浮点参考逐帧比对，误差必须恒 <=1ns。
	for tick := int64(1); tick <= 12000; tick++ {
		got := r.At(tick)
		want := int64(fit.Slope * float64(tick))
		d := got - want
		if d > 1 || d < -1 {
			t.Fatalf("tick=%d 重建误差 %dns 超过 1ns", tick, d)
		}
	}
}

func TestFitRateInsufficientAnchors(t *testing.T) {
	if _, err := FitRate(nil); err != ErrInsufficientAnchors {
		t.Fatalf("空锚点应报错, got %v", err)
	}
	if _, err := FitRate([]Anchor{{Tick: 1, NS: 2}}); err != ErrInsufficientAnchors {
		t.Fatalf("单锚点应报错, got %v", err)
	}
}
