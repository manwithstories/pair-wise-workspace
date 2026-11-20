package process

import "testing"

func TestSilenceBasic(t *testing.T) {
	// 10 桶：0-1 响，2-5 静，6-9 响
	levels := []float64{0.9, 0.9, 0.01, 0.01, 0.01, 0.01, 0.9, 0.9, 0.9, 0.9}
	iv, err := DetectSilence(levels, 100, 0.1, 0.3, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(iv) != 1 || iv[0].Start != 200 || iv[0].End != 600 {
		t.Fatalf("iv=%+v", iv)
	}
	if got := TotalTicks(iv); got != 400 {
		t.Fatalf("total=%d", got)
	}
}

func TestSilenceHysteresis(t *testing.T) {
	// 音量在低阈值下方、滞回带内抖动：进入静音后，滞回带内的波动不退出
	levels := []float64{0.9, 0.05, 0.15, 0.25, 0.15, 0.05, 0.9}
	iv, err := DetectSilence(levels, 100, 0.1, 0.3, 1)
	if err != nil {
		t.Fatal(err)
	}
	// 从桶 1 进入静音，直到桶 6（0.9 > high）退出 → 单个区间 [100,600)
	if len(iv) != 1 || iv[0].Start != 100 || iv[0].End != 600 {
		t.Fatalf("iv=%+v", iv)
	}
}

func TestSilenceWindowSmoothing(t *testing.T) {
	// 窗口平均抑制单桶尖峰：单个响桶不应打断静音
	levels := []float64{0.02, 0.02, 0.8, 0.02, 0.02}
	iv, err := DetectSilence(levels, 100, 0.1, 0.3, 3)
	if err != nil {
		t.Fatal(err)
	}
	// 窗均值最高 (0.02+0.8+0.02)/3≈0.28 < high 0.3 → 整段一个静音区间
	if len(iv) != 1 || iv[0].Start != 0 || iv[0].End != 500 {
		t.Fatalf("iv=%+v", iv)
	}
}

func TestSilenceInvalid(t *testing.T) {
	if _, err := DetectSilence([]float64{1}, 100, 0.5, 0.3, 1); err == nil {
		t.Fatal("expected threshold error")
	}
	if _, err := DetectSilence([]float64{1}, 0, 0.1, 0.3, 1); err == nil {
		t.Fatal("expected bucket error")
	}
}
