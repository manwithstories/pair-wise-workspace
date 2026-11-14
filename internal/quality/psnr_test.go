package quality

import (
	"testing"

	"turncode/internal/yuv"
)

func TestFramePSNRAndSegments(t *testing.T) {
	a, _ := yuv.NewFrame(16, 16)
	b, _ := yuv.NewFrame(16, 16)
	for i := range a.Y {
		a.Y[i] = 100
		b.Y[i] = 100
	}
	for i := range a.U {
		a.U[i], a.V[i] = 128, 128
		b.U[i], b.V[i] = 128, 128
	}
	p, err := FramePSNR(a, b)
	if err != nil || p != UnlimitedPSNR {
		t.Fatalf("identical psnr=%f err=%v", p, err)
	}
	b.Y[0] = 200
	p, err = FramePSNR(a, b)
	if err != nil || p >= UnlimitedPSNR || p <= 0 {
		t.Fatalf("damaged psnr=%f err=%v", p, err)
	}
	acc := NewAccumulator(2)
	for i, v := range []float64{40, 42, 38, 36} {
		acc.Add(i, v)
	}
	if acc.TotalFrames != 4 || len(acc.Segments) != 2 {
		t.Fatal("segment accounting mismatch")
	}
	if acc.Mean() != 39 || acc.Segments[1].MinPSNR != 36 {
		t.Fatalf("mean/min mismatch: %f", acc.Mean())
	}
	if acc.SegmentStdDB() <= 0 {
		t.Fatal("expected segment variance")
	}
}

func TestRegressionThresholds(t *testing.T) {
	base := Summary{MeanDB: 40, SegmentStdDB: 1, SegmentMeans: []float64{41, 39}}
	cur := Summary{MeanDB: 39.4, SegmentStdDB: 1, SegmentMeans: []float64{40.4, 38.4}}
	reg := Compare(cur, base)
	if !reg.Regression || len(reg.DegradedSegments) != 2 {
		t.Fatalf("mean regression not detected: %+v", reg)
	}
	cur2 := Summary{MeanDB: 40, SegmentStdDB: 1.4, SegmentMeans: []float64{43, 37}}
	if !Compare(cur2, base).Regression {
		t.Fatal("segment spread regression not detected")
	}
}
