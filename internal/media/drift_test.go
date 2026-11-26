package media

import (
	"math"
	"testing"
)

var driftTB = Timebase{1, 90000}

// 线性漂移：ref = local + 100ms + 0.001ms/tick
func linearAnchors(n int) []Anchor {
	a := make([]Anchor, n)
	for i := range a {
		pts := int64(i) * 90000
		a[i] = Anchor{PTS: pts, RefMs: driftTB.ToMillis(pts) + 100 + 0.001*float64(pts)}
	}
	return a
}

func TestFitDriftLinear(t *testing.T) {
	dc, err := FitDrift(driftTB, linearAnchors(5), 1e-4)
	if err != nil {
		t.Fatal(err)
	}
	if len(dc.Rejected) != 0 {
		t.Fatalf("unexpected rejections: %+v", dc.Rejected)
	}
	// 段内任意点校正误差应远小于 1ms
	for _, pts := range []int64{0, 45000, 180000, 359999} {
		want := driftTB.ToMillis(pts) + 100 + 0.001*float64(pts)
		if got := dc.CorrectMs(driftTB, pts); math.Abs(got-want) > 1e-6 {
			t.Fatalf("CorrectMs(%d) = %v, want %v", pts, got, want)
		}
	}
}

func TestFitDriftRejectsOutlier(t *testing.T) {
	anchors := linearAnchors(6)
	anchors[3].RefMs += 5000 // 异常锚点：偏离 5 秒
	dc, err := FitDrift(driftTB, anchors, 1e-4)
	if err != nil {
		t.Fatal(err)
	}
	if len(dc.Rejected) != 1 || dc.Rejected[0].PTS != anchors[3].PTS {
		t.Fatalf("rejected = %+v, want pts %d", dc.Rejected, anchors[3].PTS)
	}
	// 异常锚点之后的轨道段不被污染
	for _, pts := range []int64{360000, 450000} {
		want := driftTB.ToMillis(pts) + 100 + 0.001*float64(pts)
		if got := dc.CorrectMs(driftTB, pts); math.Abs(got-want) > 1e-6 {
			t.Fatalf("CorrectMs(%d) = %v, want %v (outlier polluted later track)", pts, got, want)
		}
	}
}

func TestFitDriftConstantOffset(t *testing.T) {
	dc, err := FitDrift(driftTB, []Anchor{{PTS: 9000, RefMs: 150}}, 1e-4)
	if err != nil {
		t.Fatal(err)
	}
	if got := dc.CorrectMs(driftTB, 0); got != 50 {
		t.Fatalf("CorrectMs(0) = %v, want 50", got)
	}
	if got := dc.CorrectMs(driftTB, 90000); got != 1050 {
		t.Fatalf("CorrectMs(90000) = %v, want 1050", got)
	}
}

func TestFitDriftErrors(t *testing.T) {
	if _, err := FitDrift(driftTB, nil, 1e-4); err == nil {
		t.Fatal("empty anchors accepted")
	}
	dup := []Anchor{{PTS: 1, RefMs: 1}, {PTS: 1, RefMs: 2}}
	if _, err := FitDrift(driftTB, dup, 1e-4); err == nil {
		t.Fatal("duplicate anchor pts accepted")
	}
	if _, err := FitDrift(Timebase{0, 1}, linearAnchors(2), 1e-4); err == nil {
		t.Fatal("invalid timebase accepted")
	}
}

func TestCorrectMsNil(t *testing.T) {
	var dc *DriftCorrection
	if got := dc.CorrectMs(driftTB, 90000); got != 1000 {
		t.Fatalf("nil correction = %v, want identity 1000", got)
	}
}
