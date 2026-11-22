package metrics

import "testing"

func TestObserveAndPercentile(t *testing.T) {
	h := NewHistogram(100)
	for i := 1; i <= 100; i++ {
		h.Observe(int64(i))
	}
	if h.Count() != 100 || h.Sum() != 5050 {
		t.Fatalf("count=%d sum=%d", h.Count(), h.Sum())
	}
	if p := h.Percentile(95); p != 95 {
		t.Fatalf("p95 = %d", p)
	}
	if p := h.Percentile(50); p != 50 {
		t.Fatalf("p50 = %d", p)
	}
	if p := h.Percentile(100); p != 100 {
		t.Fatalf("p100 = %d", p)
	}
}

func TestClampAndNegative(t *testing.T) {
	h := NewHistogram(10)
	h.Observe(-5)
	h.Observe(1000)
	if h.Percentile(50) != 0 && h.Percentile(100) != 10 {
		t.Fatalf("clamp broken: %d", h.Percentile(100))
	}
	if got := h.Buckets()[0]; got != 1 {
		t.Fatalf("negative not clamped to 0: %v", h.Buckets())
	}
}

func TestEmptyAndRoundTrip(t *testing.T) {
	h := NewHistogram(20)
	if h.Percentile(95) != 0 || h.Count() != 0 {
		t.Fatal("empty histogram not zero")
	}
	h.Observe(3)
	h.Observe(7)
	b := h.Buckets()
	r := NewHistogram(20)
	if err := r.Restore(b); err != nil {
		t.Fatal(err)
	}
	if r.Count() != 2 || r.Sum() != 10 || r.Percentile(50) != 3 {
		t.Fatalf("restored = %+v", r)
	}
	if err := r.Restore([]int64{1}); err == nil {
		t.Fatal("shape mismatch accepted")
	}
}
