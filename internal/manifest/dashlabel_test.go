package manifest

import (
	"strings"
	"testing"
)

// TestDASHUniqueLabelFallback covers sets whose declared id is empty or
// already taken: the fallback label must still be unique so segment ids never
// collide even without explicit ids.
func TestDASHUniqueLabelFallback(t *testing.T) {
	src := `<MPD><Period>
<AdaptationSet mimeType="video/mp4" timescale="90000"><SegmentTimeline><S t="0" d="90000"/></SegmentTimeline></AdaptationSet>
<AdaptationSet mimeType="video/mp4" timescale="90000"><SegmentTimeline><S t="0" d="90000"/></SegmentTimeline></AdaptationSet>
</Period></MPD>`
	m, err := ParseDASH(strings.NewReader(src), DefaultLimits())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(m.Segments) != 2 {
		t.Fatalf("segments = %d", len(m.Segments))
	}
	if m.Segments[0].ID == m.Segments[1].ID || m.Segments[0].URI == m.Segments[1].URI {
		t.Fatalf("unlabelled sets collided: %+v", m.Segments)
	}
}
