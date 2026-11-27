package transform

import (
	"context"
	"testing"

	"relayaudit/internal/fault"
	"relayaudit/internal/fixture"
	"relayaudit/internal/timeline"
	"relayaudit/internal/wire"
)

func buildScan(t *testing.T, spec fixture.Spec) (*fixture.Segment, *wire.ScanResult) {
	t.Helper()
	seg, err := fixture.Generate(spec)
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	scan, err := wire.Scan(seg.Bytes)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	return seg, scan
}

func TestBuildRangesMergesOverlappingAndSkipsIndexFaults(t *testing.T) {
	fs := []*fault.Fault{
		fault.New(fault.CRC, 100, "crc").WithLength(50),
		fault.New(fault.Boundary, 120, "boundary").WithLength(80),
		fault.New(fault.Index, 24, "index").WithLength(1024),
		fault.New(fault.Truncated, 900, "tail").WithLength(10),
	}
	got := BuildRanges(fs, 1000)
	if len(got) != 2 {
		t.Fatalf("ranges = %d, want 2: %+v", len(got), got)
	}
	if got[0].Start != 100 || got[0].End != 200 {
		t.Fatalf("merged range = %+v, want [100,200)", got[0])
	}
	if got[1].Start != 900 || got[1].End != 1000 {
		t.Fatalf("tail range = %+v, want [900,1000)", got[1])
	}
}

func TestBuildRangesClampsToSource(t *testing.T) {
	fs := []*fault.Fault{fault.New(fault.Truncated, 95, "tail").WithLength(50)}
	got := BuildRanges(fs, 100)
	if len(got) != 1 || got[0].End != 100 {
		t.Fatalf("range not clamped: %+v", got)
	}
}

func TestBuildRangesDropsOutOfSourceFaults(t *testing.T) {
	fs := []*fault.Fault{fault.New(fault.CRC, 500, "gone")}
	if got := BuildRanges(fs, 100); len(got) != 0 {
		t.Fatalf("out-of-source fault kept: %+v", got)
	}
}

func TestRepairIsolatesCorruptRecordAtExactOffset(t *testing.T) {
	seg, scan := buildScan(t, fixture.Spec{Seed: 73, Size: 1 << 19, CorruptRecord: 17})
	res, _, err := timeline.Resolve(scan.Records, timeline.SamplePlan(scan.Records))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	stats, out, err := Repair(context.Background(), scan, res, nil)
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	if stats.Dropped != 1 {
		t.Fatalf("dropped = %d, want 1", stats.Dropped)
	}
	if len(stats.Ranges) != 1 {
		t.Fatalf("ranges = %d, want 1", len(stats.Ranges))
	}
	if got := stats.Ranges[0].Start; got != seg.Damage.CorruptRecordOff {
		t.Fatalf("isolated offset = %d, want %d", got, seg.Damage.CorruptRecordOff)
	}
	if stats.Kept != len(seg.Records)-1 {
		t.Fatalf("kept = %d, want %d", stats.Kept, len(seg.Records)-1)
	}
	// The repaired output must scan cleanly with a trusted index.
	re, err := wire.Scan(out)
	if err != nil {
		t.Fatalf("output scan: %v", err)
	}
	if re.IndexRebuilt || len(re.Damage) != 0 {
		t.Fatalf("output not clean: rebuilt=%v damage=%v", re.IndexRebuilt, re.Damage)
	}
	if len(re.Records) != stats.Kept {
		t.Fatalf("output records = %d, want %d", len(re.Records), stats.Kept)
	}
}

func TestRepairIsolatesTruncatedTail(t *testing.T) {
	seg, scan := buildScan(t, fixture.Spec{Seed: 5, Size: 1 << 18, TruncateTail: 41})
	res, _, _ := timeline.Resolve(scan.Records, timeline.SamplePlan(scan.Records))
	stats, out, err := Repair(context.Background(), scan, res, nil)
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	if stats.Dropped != 1 {
		t.Fatalf("dropped = %d, want 1", stats.Dropped)
	}
	want := int64(len(seg.Bytes))
	if stats.Ranges[0].Start >= want {
		t.Fatalf("tail range start %d beyond file %d", stats.Ranges[0].Start, want)
	}
	if stats.Ranges[0].End != want {
		t.Fatalf("tail range end = %d, want %d", stats.Ranges[0].End, want)
	}
	if _, err := wire.Scan(out); err != nil {
		t.Fatalf("repaired output does not scan: %v", err)
	}
}

func TestRepairIsolatesBoundaryInsideRecord(t *testing.T) {
	seg, scan := buildScan(t, fixture.Spec{Seed: 12, Size: 1 << 19})
	plan := timeline.SamplePlan(scan.Records)
	// Shift a cut 1 tick into the clip's first record so it lands mid-record.
	plan.Clips[0].End++
	_, bf, err := timeline.Resolve(scan.Records, plan)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(bf) == 0 {
		t.Fatalf("expected a boundary fault from the shifted cut")
	}
	res, _, _ := timeline.Resolve(scan.Records, plan)
	stats, out, err := Repair(context.Background(), scan, res, bf)
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	found := false
	for _, r := range stats.Ranges {
		if r.Kind == fault.Boundary && r.Start == bf[0].Offset() {
			found = true
		}
	}
	if !found {
		t.Fatalf("boundary fault not isolated: ranges=%+v faults=%v", stats.Ranges, bf)
	}
	if _, err := wire.Scan(out); err != nil {
		t.Fatalf("output scan: %v", err)
	}
	_ = seg
}

func TestRepairCountsDamageRangesForFullDamageSet(t *testing.T) {
	_, scan := buildScan(t, fixture.Spec{
		Seed: 73, Size: 1 << 19, CorruptRecord: 17, TruncateTail: 41,
	})
	res, _, _ := timeline.Resolve(scan.Records, timeline.SamplePlan(scan.Records))
	stats, _, err := Repair(context.Background(), scan, res, nil)
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	if len(stats.Ranges) != 2 {
		t.Fatalf("damage ranges = %d, want 2: %+v", len(stats.Ranges), stats.Ranges)
	}
}

func TestRepairHonoursCancellation(t *testing.T) {
	_, scan := buildScan(t, fixture.Spec{Seed: 9, Size: 1 << 19})
	res, _, _ := timeline.Resolve(scan.Records, timeline.SamplePlan(scan.Records))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := Repair(ctx, scan, res, nil)
	if !fault.IsKind(err, fault.Canceled) {
		t.Fatalf("err = %v, want canceled", err)
	}
}

func TestEncoderRestoreReparsesPrefix(t *testing.T) {
	seg, scan := buildScan(t, fixture.Spec{Seed: 3, Size: 1 << 18})
	e := NewEncoder()
	for i, rec := range scan.Records[:10] {
		e.Add(rec)
		_ = i
	}
	prefix := append([]byte(nil), e.Bytes()...)

	fresh := NewEncoder()
	if err := fresh.Restore(prefix); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if fresh.Kept() != 10 {
		t.Fatalf("restored records = %d, want 10", fresh.Kept())
	}
	for i, rec := range fresh.Records() {
		if rec.Offset != scan.Records[i].Offset && rec.Offset == 0 && i > 0 {
			t.Fatalf("record %d has no output offset", i)
		}
		if rec.Seq != scan.Records[i].Seq {
			t.Fatalf("record %d seq = %d, want %d", i, rec.Seq, scan.Records[i].Seq)
		}
	}
	for _, rec := range scan.Records[10:] {
		fresh.Add(rec)
	}
	out, err := fresh.Finalize(uint8(scan.Header.Tracks))
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	re, err := wire.Scan(out)
	if err != nil {
		t.Fatalf("rescan: %v", err)
	}
	if len(re.Records) != len(seg.Records) {
		t.Fatalf("resumed output has %d records, want %d", len(re.Records), len(seg.Records))
	}
}

func TestEncoderRestoreRejectsGarbage(t *testing.T) {
	e := NewEncoder()
	if err := e.Restore([]byte("not records")); !fault.IsKind(err, fault.Checkpoint) {
		t.Fatalf("garbage prefix accepted: %v", err)
	}
}

func TestAnnotateMapsOffsetsToClips(t *testing.T) {
	_, scan := buildScan(t, fixture.Spec{Seed: 73, Size: 1 << 19})
	res, _, err := timeline.Resolve(scan.Records, timeline.SamplePlan(scan.Records))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	var target *wire.Record
	for _, rec := range scan.Records {
		if tick, ok := res.TickForOffset(rec.Track, rec.Offset); ok && tick > 0 && len(res.LabelsFor(rec)) > 0 {
			target = rec
			break
		}
	}
	if target == nil {
		t.Fatalf("no record carries a clip label past tick 0")
	}
	ranges := []Range{{Start: target.Offset, End: target.End(), Kind: fault.CRC, Track: target.Track}}
	Annotate(ranges, res)
	if len(ranges[0].Clips) == 0 {
		t.Fatalf("record at %d (tick %d) not mapped to any clip", target.Offset, ranges[0].Ticks)
	}
	if ranges[0].Ticks == 0 {
		t.Fatalf("range tick not populated")
	}
}
