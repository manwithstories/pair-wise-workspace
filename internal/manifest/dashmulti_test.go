package manifest

import (
	"strings"
	"testing"
)

func TestDASHMultipleAdaptationSetsUniqueIDs(t *testing.T) {
	src := `<MPD><Period>
<AdaptationSet id="v" mimeType="video/mp4" timescale="90000"><SegmentTimeline><S t="0" d="180000" r="2"/></SegmentTimeline></AdaptationSet>
<AdaptationSet id="a" contentType="audio" timescale="48000"><SegmentTimeline><S t="0" d="96000" r="2"/></SegmentTimeline></AdaptationSet>
</Period></MPD>`
	m, err := ParseDASH(strings.NewReader(src), DefaultLimits())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ids := map[string]bool{}
	for _, s := range m.Segments {
		if ids[s.ID] {
			t.Fatalf("duplicate segment id %q across adaptation sets", s.ID)
		}
		ids[s.ID] = true
	}
	if len(ids) != 6 {
		t.Fatalf("segment ids = %d, want 6", len(ids))
	}
}

// TestDASHTwoVideoSetsGetDistinctIDs guards a regression where two video
// AdaptationSets both fell back to the "video" label and emitted colliding
// segment ids with identical URIs, which the duplicate check then accepted
// because the content commitments matched.
func TestDASHTwoVideoSetsGetDistinctIDs(t *testing.T) {
	src := `<MPD><Period>
<AdaptationSet id="v0" mimeType="video/mp4" timescale="90000"><SegmentTimeline><S t="0" d="90000"/></SegmentTimeline></AdaptationSet>
<AdaptationSet id="v1" mimeType="video/mp4" timescale="90000"><SegmentTimeline><S t="0" d="90000"/></SegmentTimeline></AdaptationSet>
</Period></MPD>`
	m, err := ParseDASH(strings.NewReader(src), DefaultLimits())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(m.Segments) != 2 {
		t.Fatalf("segments = %d, want 2", len(m.Segments))
	}
	if m.Segments[0].ID == m.Segments[1].ID {
		t.Fatalf("two video sets share segment id %q", m.Segments[0].ID)
	}
	if m.Segments[0].URI == m.Segments[1].URI {
		t.Fatalf("two video sets share URI %q", m.Segments[0].URI)
	}
}
