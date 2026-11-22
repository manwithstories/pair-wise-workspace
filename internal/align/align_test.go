package align

import (
	"testing"

	"mediaroute/internal/adapter"
	"mediaroute/internal/fault"
)

func TestValidateReportsTimingFaults(t *testing.T) {
	events := []adapter.Event{
		{Track: "s", Seq: 1, TimestampNs: 100},
		{Track: "s", Seq: 2, TimestampNs: 50},                 // non-monotonic
		{Track: "s", Seq: 3, TimestampNs: 200, DurationNs: -1}, // negative duration
		{Track: "s", Seq: 4, TimestampNs: 300},
	}
	out, errs := Validate(events)
	if len(out) != 2 || len(errs) != 2 {
		t.Fatalf("out=%d errs=%d", len(out), len(errs))
	}
	for _, e := range errs {
		if e.Category != fault.CatTiming || e.Track != "s" || !e.HasSeq {
			t.Fatalf("bad fault: %+v", e)
		}
	}
	if errs[0].Seq != 2 || errs[1].Seq != 3 {
		t.Fatalf("seqs: %d, %d", errs[0].Seq, errs[1].Seq)
	}
}

func TestAssignBoundaryBelongsToOneSegment(t *testing.T) {
	bounds := []int64{0, 100, 200, 300}
	if i := Assign(bounds, 100); i != 1 {
		t.Fatalf("boundary event assigned to %d, want 1", i)
	}
	if i := Assign(bounds, 0); i != 0 {
		t.Fatalf("first boundary assigned to %d", i)
	}
	if i := Assign(bounds, 299); i != 2 {
		t.Fatalf("299 assigned to %d", i)
	}
	if i := Assign(bounds, 300); i != -1 {
		t.Fatalf("end assigned to %d, want -1", i)
	}
	if i := Assign(bounds, -1); i != -1 {
		t.Fatalf("before start assigned to %d", i)
	}
}

func TestProject(t *testing.T) {
	bounds := []int64{0, 100, 200}
	events := []adapter.Event{
		{Track: "c", TimestampNs: 50},
		{Track: "c", TimestampNs: 100}, // boundary → segment 1 only
		{Track: "c", TimestampNs: 500}, // outside
	}
	segs, dropped := Project(events, bounds)
	if dropped != 1 || len(segs[0]) != 1 || len(segs[1]) != 1 {
		t.Fatalf("segs=%v dropped=%d", segs, dropped)
	}
}

func TestAlignWithinThreshold(t *testing.T) {
	targets := []int64{2_000_000_000, 4_000_000_000}
	kf := map[string][]int64{
		"720p": {2_000_000_000, 4_000_000_000},
		"480p": {2_010_000_000, 3_990_000_000}, // ±10ms
	}
	bs, errs := Align(targets, kf, "720p", 500_000_000)
	if len(errs) != 0 || len(bs) != 2 {
		t.Fatalf("bs=%+v errs=%v", bs, errs)
	}
	if bs[0].ErrorNs != 10_000_000 || bs[1].ErrorNs != 10_000_000 {
		t.Fatalf("errors: %+v", bs)
	}
}

func TestAlignRejectsBeyondThreshold(t *testing.T) {
	targets := []int64{2_000_000_000}
	kf := map[string][]int64{
		"720p": {2_000_000_000},
		"480p": {2_100_000_000}, // 100ms off
	}
	_, errs := Align(targets, kf, "720p", 500_000_000)
	if len(errs) != 1 || errs[0].Category != fault.CatAlign {
		t.Fatalf("errs=%v", errs)
	}
}

func TestAlignMissingKeyframeDelayed(t *testing.T) {
	targets := []int64{2_000_000_000}
	kf := map[string][]int64{"only": {5_000_000_000}}
	bs, errs := Align(targets, kf, "only", 500_000_000)
	if len(errs) != 0 || len(bs) != 1 || !bs[0].Delayed || bs[0].ChosenNs != 5_000_000_000 {
		t.Fatalf("bs=%+v errs=%v", bs, errs)
	}
	if bs[0].DelayCause == "" {
		t.Fatal("delay cause not recorded")
	}
}

func TestP95(t *testing.T) {
	if P95(nil) != 0 {
		t.Fatal("nil p95")
	}
	vals := []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 100}
	if P95(vals) != 100 {
		t.Fatalf("p95 = %d", P95(vals))
	}
}
