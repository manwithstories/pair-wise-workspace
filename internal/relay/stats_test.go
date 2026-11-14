package relay

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStatsSnapshotAndQuantiles(t *testing.T) {
	stats := NewStats()
	stats.SetEmitted(100)
	for i := 0; i < 95; i++ {
		stats.IncReceived()
		stats.IncUnique()
		stats.IncForwarded(100)
	}
	for i := 0; i < 5; i++ {
		stats.IncDuplicate()
	}
	for _, sample := range []float64{0, 1, 2, 3, 100} {
		stats.RecordJitter(sample)
	}
	metrics := stats.Snapshot()
	if metrics.Unique != 95 || metrics.Duplicates != 5 || metrics.Received != metrics.Unique {
		t.Fatalf("metrics = %+v", metrics)
	}
	if metrics.LossRatio < 0.049 || metrics.LossRatio > 0.051 {
		t.Fatalf("loss ratio = %f", metrics.LossRatio)
	}
	if metrics.ForwardRatio < 0.949 || metrics.ForwardRatio > 0.951 {
		t.Fatalf("forward ratio = %f", metrics.ForwardRatio)
	}
	if metrics.JitterMS.P50 != 2 || metrics.JitterMS.P95 != 100 || metrics.JitterMS.Max != 100 {
		t.Fatalf("jitter quantiles = %+v", metrics.JitterMS)
	}
}

func TestReportWriteReadAndInvalid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "report.json")
	metrics := Metrics{Emitted: 10, Received: 9, Unique: 9, Forwarded: 9, Duplicates: 0, ForwardBytes: 100, LossRatio: 0.1, ForwardRatio: 0.9}
	report := NewReport(42, 10, time.Second, 90000, 100, "127.0.0.1:9", "127.0.0.1:1/udp", 0, metrics)
	if err := WriteReport(path, report); err != nil {
		t.Fatal(err)
	}
	got, err := ReadReport(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Emitted != 10 || got.Unique != 9 || got.SelectedPair != "127.0.0.1:1/udp" {
		t.Fatalf("report = %+v", got)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadReport(path); !errors.Is(err, ErrInvalidReport) {
		t.Fatalf("error = %v", err)
	}
	invalidCounters := report
	invalidCounters.Forwarded = invalidCounters.Unique + 1
	invalidPath := filepath.Join(dir, "invalid.json")
	if err := WriteReport(invalidPath, invalidCounters); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadReport(invalidPath); !errors.Is(err, ErrInvalidReport) {
		t.Fatalf("inconsistent counters error = %v", err)
	}
}

func TestQuantileNearestRank(t *testing.T) {
	samples := []float64{1, 2, 3, 4}
	if got := Quantile(samples, .5); got != 2 {
		t.Fatalf("p50 = %f", got)
	}
	if got := Quantile(samples, .95); got != 4 {
		t.Fatalf("p95 = %f", got)
	}
	if got := Quantile(nil, .95); got != 0 {
		t.Fatalf("empty = %f", got)
	}
}
