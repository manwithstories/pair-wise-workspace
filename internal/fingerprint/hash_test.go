package fingerprint

import (
	"testing"

	"mediad/internal/clock"
	"mediad/internal/codec"
	"mediad/internal/timeline"
)

func unit(start, dur int64, payload ...byte) codec.AccessUnit {
	return codec.AccessUnit{
		Start:   clock.At(start, clock.Base48k),
		Dur:     clock.At(dur, clock.Base48k),
		Payload: payload,
	}
}

func TestHashStableAndSensitive(t *testing.T) {
	a := []codec.AccessUnit{unit(0, 960, 1, 2, 3)}
	b := []codec.AccessUnit{unit(0, 960, 1, 2, 3)}
	c := []codec.AccessUnit{unit(0, 960, 1, 2, 4)}
	if HashUnits(a) != HashUnits(b) {
		t.Fatal("identical content must hash identically")
	}
	if HashUnits(a) == HashUnits(c) {
		t.Fatal("different payload must change the hash")
	}
	if len(HashUnits(a)) != 64 {
		t.Fatalf("hash length %d, want 64 hex chars", len(HashUnits(a)))
	}
}

func TestSegmentHashDeterministic(t *testing.T) {
	seg := timeline.Segment{
		Index: 0,
		Units: map[string][]codec.AccessUnit{
			"b": {unit(0, 960, 9)},
			"a": {unit(0, 960, 1)},
		},
	}
	if HashSegment(seg) != HashSegment(seg) {
		t.Fatal("segment hash must be deterministic regardless of map order")
	}
}

func TestDedupKeepsSourceMapping(t *testing.T) {
	segs := []timeline.Segment{
		{Index: 0, Units: map[string][]codec.AccessUnit{
			"studio": {unit(0, 960, 7, 7)},
			"remote": {unit(0, 960, 7, 7)}, // same content, other track
			"field":  {unit(0, 960, 3)},
		}},
	}
	d := Dedup(segs)
	if len(d) != 2 {
		t.Fatalf("dedup groups=%d, want 2", len(d))
	}
	dup := HashUnits([]codec.AccessUnit{unit(0, 960, 7, 7)})
	srcs := d[dup]
	if len(srcs) != 2 {
		t.Fatalf("sources=%v, want studio+remote", srcs)
	}
	seen := map[string]bool{}
	for _, s := range srcs {
		seen[s.Track] = true
		if s.Segment != 0 {
			t.Fatalf("segment=%d, want 0", s.Segment)
		}
	}
	if !seen["studio"] || !seen["remote"] {
		t.Fatalf("source mapping lost: %v", srcs)
	}
}
