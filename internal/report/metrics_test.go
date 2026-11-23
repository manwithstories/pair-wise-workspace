package report

import "testing"

func TestP95(t *testing.T) {
	if P95(nil) != 0 {
		t.Fatal("empty input must give 0")
	}
	var vals []float64
	for i := 1; i <= 100; i++ {
		vals = append(vals, float64(i))
	}
	if got := P95(vals); got != 95 {
		t.Fatalf("p95=%v, want 95", got)
	}
	if got := P95([]float64{7}); got != 7 {
		t.Fatalf("p95=%v", got)
	}
}

func TestTicksToMS(t *testing.T) {
	if got := TicksToMS(90000); got != 1000 {
		t.Fatalf("got %v", got)
	}
	if got := TicksToMS(-45); got != 0.5 {
		t.Fatalf("got %v", got)
	}
}
