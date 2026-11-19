package report_test

import (
	"testing"

	"archivearchivist/internal/model"
	"archivearchivist/internal/report"
)

func TestQuantileNearestRank(t *testing.T) {
	xs := make([]int64, 100)
	for i := range xs {
		xs[i] = int64(i + 1)
	}
	if got := report.Quantile(xs, 95); got != 95 {
		t.Fatalf("p95 = %d, want 95", got)
	}
	if got := report.Quantile(xs, 50); got != 50 {
		t.Fatalf("p50 = %d, want 50", got)
	}
	if got := report.Quantile(xs, 100); got != 100 {
		t.Fatalf("p100 = %d, want 100", got)
	}
	if got := report.Quantile(nil, 95); got != 0 {
		t.Fatalf("empty p95 = %d", got)
	}
	// p95 of 20 values is the 19th, by nearest rank.
	twenty := []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20}
	if got := report.Quantile(twenty, 95); got != 19 {
		t.Fatalf("p95 of 20 = %d, want 19", got)
	}
}

func TestQuantileIsOrderIndependent(t *testing.T) {
	a := []int64{5, 1, 4, 2, 3}
	b := []int64{1, 2, 3, 4, 5}
	if report.Quantile(a, 60) != report.Quantile(b, 60) {
		t.Fatalf("quantile depends on input order")
	}
	// The caller's slice must not be reordered.
	if a[0] != 5 {
		t.Fatalf("Quantile mutated its input: %v", a)
	}
}

func TestMean(t *testing.T) {
	if got := report.Mean([]int64{10, 20, 31}); got != 20 {
		t.Fatalf("mean = %d", got)
	}
	if got := report.Mean(nil); got != 0 {
		t.Fatalf("empty mean = %d", got)
	}
}

func TestFormatMillis(t *testing.T) {
	cases := map[int64]string{
		0: "0.000", 1: "0.001", 999: "0.999", 1000: "1.000",
		1234567: "1234.567", -1500: "-1.500",
	}
	for in, want := range cases {
		if got := report.FormatMillis(in); got != want {
			t.Fatalf("FormatMillis(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestFormatRate(t *testing.T) {
	if got := report.FormatRate(model.NewRat(233, 240)); got != "0.971" {
		t.Fatalf("rate = %q", got)
	}
	if got := report.FormatRate(model.NewRat(1, 1)); got != "1.000" {
		t.Fatalf("rate = %q", got)
	}
}
