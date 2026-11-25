package report

import (
	"bytes"
	"strings"
	"testing"

	"relaycheck/internal/model"
)

func TestSummaryAggregation(t *testing.T) {
	b := NewBuilder()
	b.AddManifest(model.ManifestReport{Faults: []model.Fault{
		{Kind: model.FaultManifestMissing, Segment: 5},
	}})
	b.AddFragment(model.FragmentReport{
		Seq: 0, FirstBadOff: -1,
		Samples:   []model.SampleInfo{{Index: 0}, {Index: 1}},
		SyncTimes: []model.Rat{model.Nano(0)},
	})
	b.AddFragment(model.FragmentReport{
		Seq: 1, FirstBadOff: 224,
		Samples:   []model.SampleInfo{{Index: 0}},
		SyncTimes: []model.Rat{model.Nano(1_000_000_000)},
		Faults:    []model.Fault{{Kind: model.FaultBoxTruncated, Segment: 1, Offset: 224}},
	})
	b.AddRTP(model.RTPReport{Faults: []model.Fault{
		{Kind: model.FaultRTPWrap, Segment: 9, Record: 3},
	}})
	s := b.Finalize(4096, 0, "abc")
	if s.Segments != 2 || s.Samples != 3 {
		t.Fatalf("seg=%d samples=%d", s.Segments, s.Samples)
	}
	if s.FirstBad != 224 || s.FirstBadSegment != 1 {
		t.Fatalf("first_bad=%d seg=%d", s.FirstBad, s.FirstBadSegment)
	}
	if len(s.KeyframeGapMS) != 1 || s.KeyframeGapMS[0] != 1000 {
		t.Fatalf("gaps=%v", s.KeyframeGapMS)
	}
	if s.FaultCount != 3 || s.MemoryPeakBytes != 4096 || s.SHA256 != "abc" {
		t.Fatalf("summary=%+v", s)
	}
}

func TestRenderContainsRequiredFields(t *testing.T) {
	b := NewBuilder()
	s := b.Finalize(1024, 7, "deadbeef")
	var buf bytes.Buffer
	Render(&buf, s)
	out := buf.String()
	for _, key := range []string{
		"segments=", "samples=", "first_bad=", "offset_delta_bytes=",
		"keyframe_gap_ms=", "memory_peak_bytes=", "resume_ms=", "sha256=",
	} {
		if !strings.Contains(out, key) {
			t.Fatalf("missing field %s in:\n%s", key, out)
		}
	}
}

func TestNoFaultFirstBadMinusOne(t *testing.T) {
	b := NewBuilder()
	s := b.Finalize(0, 0, "x")
	if s.FirstBad != -1 || s.FirstBadSegment != -1 {
		t.Fatalf("clean run first_bad must be -1, got %d/%d", s.FirstBad, s.FirstBadSegment)
	}
}
