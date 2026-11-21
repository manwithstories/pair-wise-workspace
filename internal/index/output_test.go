package index

import (
	"testing"

	"archivecheck.local/archivecheck/internal/remux"
)

func TestFromOutputKeepsKeyframesOnly(t *testing.T) {
	sum := &remux.Summary{
		MdatOffset: 100,
		Manifest: []remux.ManifestEntry{
			{Track: 1, OutputSample: 1, Keyframe: true, Tick: 0},
			{Track: 1, OutputSample: 2, Keyframe: false, Tick: 40},
			{Track: 1, OutputSample: 3, Keyframe: true, Tick: 80},
			{Track: 2, OutputSample: 1, Keyframe: true, Tick: 0},
		},
		SampleOffsets: map[uint64][]int64{1: {110, 120, 130}, 2: {200}},
	}
	ix := FromOutput(sum)
	if len(ix.Entries) != 3 {
		t.Fatalf("entries=%d", len(ix.Entries))
	}
	if ix.Entries[0].OutputSample != 1 || ix.Entries[0].SourceOffset != 110 || ix.Entries[0].Tick != 0 {
		t.Fatalf("first entry=%+v", ix.Entries[0])
	}
	if ix.Entries[1].OutputSample != 3 || ix.Entries[1].SourceOffset != 130 {
		t.Fatalf("second entry=%+v", ix.Entries[1])
	}
	if e, ok := ix.AtOrBefore(1, 90); !ok || e.OutputSample != 3 {
		t.Fatalf("lookup failed: %+v %v", e, ok)
	}
}
