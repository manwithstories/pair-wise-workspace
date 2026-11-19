package index

import (
	"testing"

	"mediapipeline/internal/mediaerr"
)

func TestBuildAndOffsets(t *testing.T) {
	samples := []Sample{
		{IsKeyframe: true, Size: 10},
		{IsKeyframe: false, Size: 20},
		{IsKeyframe: true, Size: 30},
	}
	idx, merr := Build(5, samples)
	if merr != nil {
		t.Fatal(merr)
	}
	entries := idx.Entries()
	if len(entries) != 2 {
		t.Fatalf("entries=%d want 2", len(entries))
	}
	if entries[0].SampleNumber != 1 || entries[0].Offset != 0 {
		t.Fatalf("first entry wrong: %+v", entries[0])
	}
	if entries[1].SampleNumber != 3 || entries[1].Offset != 30 {
		t.Fatalf("second entry offset must be 10+20=30: %+v", entries[1])
	}
	if idx.SampleCount() != 3 || idx.PayloadSize() != 60 {
		t.Fatalf("manifest totals wrong: count=%d size=%d", idx.SampleCount(), idx.PayloadSize())
	}
}

func TestNoKeyframeReported(t *testing.T) {
	idx, merr := Build(9, []Sample{{Size: 5}, {Size: 7}})
	if merr == nil || merr.Kind != mediaerr.KindNoKeyframe || merr.Track != 9 {
		t.Fatalf("expected locatable no-keyframe error, got %v", merr)
	}
	if len(idx.Entries()) != 0 || idx.SampleCount() != 2 {
		t.Fatal("empty index must still expose the sample manifest")
	}
}

func TestValidateOutOfRange(t *testing.T) {
	idx, _ := Build(1, []Sample{
		{IsKeyframe: true, Size: 4},
		{IsKeyframe: false, Size: 4},
	})
	// Valid entry passes.
	if merr := idx.Validate([]Entry{{SampleNumber: 1, Offset: 0}}); merr != nil {
		t.Fatalf("valid entry rejected: %v", merr)
	}
	// Sample number beyond the range.
	if merr := idx.Validate([]Entry{{SampleNumber: 9, Offset: 0}}); merr == nil ||
		merr.Kind != mediaerr.KindIndexOutOfRange {
		t.Fatalf("out-of-range sample must error, got %v", merr)
	}
	// Zero sample number.
	if merr := idx.Validate([]Entry{{SampleNumber: 0, Offset: 0}}); merr == nil {
		t.Fatal("zero sample number must error")
	}
	// Offset beyond the payload span.
	if merr := idx.Validate([]Entry{{SampleNumber: 1, Offset: 8}}); merr == nil {
		t.Fatal("out-of-range offset must error")
	}
}

func TestHitRateNoFalseHits(t *testing.T) {
	idx, _ := Build(1, []Sample{
		{IsKeyframe: true, Size: 1},
		{IsKeyframe: false, Size: 1},
		{IsKeyframe: true, Size: 1},
	})
	// Querying only genuine keyframes yields a perfect rate.
	hits, rate := idx.HitRate([]uint32{1, 3})
	if hits != 2 || rate != 1.0 {
		t.Fatalf("true keyframe rate wrong: hits=%d rate=%f", hits, rate)
	}
	// A fabricated sample number must be a miss, never a false hit.
	hits, rate = idx.HitRate([]uint32{1, 2, 3, 7})
	if hits != 2 || rate != 0.5 {
		t.Fatalf("false hit not suppressed: hits=%d rate=%f", hits, rate)
	}
	if _, rate = idx.HitRate(nil); rate != 0 {
		t.Fatal("empty query must report zero rate")
	}
}

func TestNegativeSize(t *testing.T) {
	if _, merr := Build(1, []Sample{{IsKeyframe: true, Size: -1}}); merr == nil ||
		merr.Kind != mediaerr.KindInvalidInput {
		t.Fatalf("negative size must error, got %v", merr)
	}
}
