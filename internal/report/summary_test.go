package report

import (
	"bytes"
	"strings"
	"testing"
)

func TestPrintKeyValueFormat(t *testing.T) {
	s := Summary{
		RoundtripPct:   100,
		KeyframeHitPct: 96.5,
		QueuePeak:      8,
		DriftPPM:       120.2,
		Segments: []SegmentEntry{
			{ID: 0, Hash: [32]byte{1}, DuplicateOf: -1},
			{ID: 1, Hash: [32]byte{1}, DuplicateOf: 0, Uncertain: true},
		},
		DuplicateSegments: 1,
	}
	var buf bytes.Buffer
	s.Print(&buf)
	out := buf.String()
	for _, key := range []string{
		"roundtrip_consistency_pct=100.0",
		"keyframe_index_hit_pct=96.5",
		"segments=2",
		"segment_sha256_count=2",
		"unique_sha256_count=1",
		"queue_peak=8",
		"drift_ppm=120",
		"segment[0].sha256=01000000",
		"dup_of=0",
		"uncertain=1",
	} {
		if !strings.Contains(out, key) {
			t.Errorf("output missing %q\n%s", key, out)
		}
	}
}

func TestUniqueHashes(t *testing.T) {
	s := Summary{Segments: []SegmentEntry{
		{Hash: [32]byte{1}}, {Hash: [32]byte{2}}, {Hash: [32]byte{1}},
	}}
	if s.UniqueHashes() != 2 {
		t.Fatalf("unique=%d", s.UniqueHashes())
	}
}
