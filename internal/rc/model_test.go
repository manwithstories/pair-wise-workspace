package rc

import (
	"math"
	"testing"
)

func TestFitPowerLaw(t *testing.T) {
	// 构造 R = 2e6 · Q^-0.8 的采样点
	qs := []float64{4, 14, 28, 48}
	rs := make([]float64, len(qs))
	for i, q := range qs {
		rs[i] = 2e6 * math.Pow(q, -0.8)
	}
	m := Fit(qs, rs)
	if math.Abs(m.B-0.8) > 0.05 {
		t.Fatalf("拟合斜率偏差过大: B=%.3f", m.B)
	}
	got := m.SolveQP(2e6 * math.Pow(20, -0.8))
	if math.Abs(got-20) > 1.0 {
		t.Fatalf("反解 QP 偏差过大: %.2f", got)
	}
}

func TestBuildPlanCarryover(t *testing.T) {
	p, err := BuildPlan("a.yuv", 320, 240, 25, 2, 110, 900000, ModeCapped)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Segments) != 3 {
		t.Fatalf("段数错误: %d", len(p.Segments))
	}
	var sum int64
	for _, s := range p.Segments {
		sum += s.TargetBits
	}
	want := int64(900000) * 110 / 25
	if diff := sum - want; diff < -3 || diff > 0 {
		t.Fatalf("缓冲结余结转错误: sum=%d want=%d", sum, want)
	}
	if _, err := BuildPlan("a.yuv", 320, 240, 25, 2, 110, 900000, "bogus"); err == nil {
		t.Fatal("非法模式未报错")
	}
}
