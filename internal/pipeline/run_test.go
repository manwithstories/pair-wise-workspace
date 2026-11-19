package pipeline

import (
	"reflect"
	"testing"

	"archive.local/recover/internal/media"
	"archive.local/recover/internal/stream"
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

// TestKeyframeSamplesWhenUnitIsNotSample covers the case where an access unit
// contains more than one NAL: scanner unit indexes then differ from sample
// indexes, and cut selection must use the sample indexes.
func TestKeyframeSamplesWhenUnitIsNotSample(t *testing.T) {
	// Two samples. Sample 0 holds a SEI (type 6) and an IDR (type 5); sample 1
	// holds a single non-IDR slice. Unit indexes are 0(SEI),1(IDR),2(slice),
	// so the only keyframe unit is unit 1, but the only keyframe sample is 0.
	var data []byte
	appendNAL := func(nalType byte, n int) {
		data = append(data, 0, 0, 0, 1, nalType)
		data = append(data, make([]byte, n)...)
	}
	appendNAL(0x06, 3) // SEI, unit 0
	first := len(data)
	appendNAL(0x65, 9) // IDR, unit 1
	appendNAL(0x41, 5) // non-IDR slice, unit 2
	seiLen := first    // sample 0 = SEI + IDR
	sampleOne := len(data) - 14

	tr := &media.ParsedTrack{
		ID: 1, Sizes: []uint32{uint32(sampleOne), uint32(len(data) - sampleOne)},
	}
	scan, err := stream.Scan(stream.H264, data)
	if err != nil {
		t.Fatal(err)
	}
	if len(scan.Keyframes) != 1 || scan.Keyframes[0] != 1 {
		t.Fatalf("fixture keyframe unit=%v want [1]", scan.Keyframes)
	}
	got := keyframeSamples(tr, scan)
	if len(got) != 1 || got[0] != 0 {
		t.Fatalf("keyframe samples=%v want [0] (unit index must not be used as sample index)", got)
	}
	_ = seiLen
}
