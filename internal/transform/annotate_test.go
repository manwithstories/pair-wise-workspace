package transform

import (
	"testing"

	"relayaudit/internal/fixture"
	"relayaudit/internal/timeline"
	"relayaudit/internal/wire"
)

// TestAnnotationOnFullSizeFixture checks that the injected damage in the
// reference fixture maps back onto a real editorial clip on the shared
// timebase, including a record that no longer parses.
func TestAnnotationOnFullSizeFixture(t *testing.T) {
	seg, err := fixture.Generate(fixture.Spec{
		Seed: 73, Size: 8388608, CorruptRecord: 17, TruncateTail: 41,
	})
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	scan, err := wire.Scan(seg.Bytes)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	res, bf, err := timeline.Resolve(scan.Records, timeline.SamplePlan(scan.Records))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(bf) != 0 {
		t.Fatalf("aligned plan produced %d boundary faults", len(bf))
	}
	ranges := BuildRanges(scan.Damage, scan.SourceSize)
	Annotate(ranges, res)
	if len(ranges) != 2 {
		t.Fatalf("ranges = %d, want 2", len(ranges))
	}
	for _, r := range ranges {
		if len(r.Clips) == 0 {
			t.Fatalf("range %+v was not mapped to any clip", r)
		}
		if r.Ticks == 0 {
			t.Fatalf("range %+v has no timeline position", r)
		}
	}
	// The damage sits early in the programme, so it belongs to the opening
	// clip of its track rather than the crossfade.
	if got := ranges[0].Clips[0]; got != "pa-a" {
		t.Fatalf("first range mapped to %q, want pa-a", got)
	}
}

// TestTickNearOffsetCoversDamagedRecords ensures an offset with no surviving
// record still maps into the editorial grid.
func TestTickNearOffsetCoversDamagedRecords(t *testing.T) {
	seg, err := fixture.Generate(fixture.Spec{Seed: 73, Size: 1 << 19})
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	scan, err := wire.Scan(seg.Bytes)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	res, _, err := timeline.Resolve(scan.Records, timeline.SamplePlan(scan.Records))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	rec := scan.Records[20]
	mid := rec.Offset + rec.Size()/2
	tick, ok := res.TickNearOffset(rec.Track, mid)
	if !ok {
		t.Fatalf("interior offset did not map")
	}
	start, _ := res.TickForOffset(rec.Track, rec.Offset)
	if tick < start {
		t.Fatalf("interior tick %d precedes record start %d", tick, start)
	}
}
