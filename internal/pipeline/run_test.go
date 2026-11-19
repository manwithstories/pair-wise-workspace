package pipeline

import (
	"reflect"
	"testing"

	"archive.local/recover/internal/testkit"
)

func TestDemoDeterministic(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DurationNs = 30_000_000_000
	a, err := Run(cfg)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Run(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a.Report.Metrics, b.Report.Metrics) {
		t.Fatalf("metrics differ between identical runs:\n%+v\n%+v", a.Report.Metrics, b.Report.Metrics)
	}
	if a.Report.RunID != b.Report.RunID {
		t.Fatalf("run id differs: %q vs %q", a.Report.RunID, b.Report.RunID)
	}
}

func TestCorruptIndexLocatesBadOffset(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Case = testkit.CaseCorruptIndex
	cfg.DurationNs = 60_000_000_000
	res, err := Run(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r := res.Report
	if r.Index == nil || r.Index.Dropped == 0 {
		t.Fatalf("corrupt index not detected: %+v", r.Index)
	}
	// The offending offset must be reported with a location an operator can
	// act on: sample index and byte offset.
	for _, i := range r.Issues {
		if i.Code == "index.offset_out_of_range" && i.Loc.Sample >= 0 && i.Loc.Offset >= 0 {
			return
		}
	}
	t.Fatalf("no located out-of-range finding: %v", r.SortedIssueCodes())
}

func TestWrapVFRDriftKeepsAllSamples(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Case = testkit.CaseWrapVFRDrift
	cfg.DurationNs = 60_000_000_000
	res, err := Run(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r := res.Report
	if r.Metrics.SyncErrorMs > 20 {
		t.Fatalf("sync error %.3f ms", r.Metrics.SyncErrorMs)
	}
	if r.Timeline == nil || r.Timeline.Samples == 0 {
		t.Fatal("no timeline samples")
	}
	if r.Metrics.Segments <= 0 {
		t.Fatal("no segments")
	}
}
