package manifest

import (
	"strings"
	"testing"
)

const sampleHLS = `#EXTM3U
#EXT-X-VERSION:7
#EXT-X-TARGETDURATION:2
#EXT-X-MEDIA-SEQUENCE:907
#EXT-X-MAP:URI="init.mp4"
#EXTINF:2.000,
seg_000.m4s
#EXTINF:2.000,
seg_001.m4s
#EXT-X-DISCONTINUITY
#EXTINF:2.000,
seg_002.m4s
#EXTINF:1.500,
seg_003.m4s
`

func TestParseHLSDiscontinuityMapSequence(t *testing.T) {
	m, err := ParseHLS(strings.NewReader(sampleHLS))
	if err != nil {
		t.Fatal(err)
	}
	if m.Kind != KindHLS || m.MediaSequence != 907 {
		t.Fatalf("kind/seq = %s/%d", m.Kind, m.MediaSequence)
	}
	if len(m.Segments) != 4 {
		t.Fatalf("segments=%d want 4", len(m.Segments))
	}
	if m.Segments[0].Sequence != 907 || m.Segments[3].Sequence != 910 {
		t.Fatalf("sequence numbering wrong: %d..%d", m.Segments[0].Sequence, m.Segments[3].Sequence)
	}
	if m.Segments[0].MapURI != "init.mp4" {
		t.Fatalf("map uri=%q", m.Segments[0].MapURI)
	}
	if !m.Segments[2].Discontinuity || m.Segments[0].Discontinuity || m.Segments[3].Discontinuity {
		t.Fatal("discontinuity must attach only to segment 2")
	}
	if m.Segments[3].DurationSec != 1.5 {
		t.Fatalf("duration=%v", m.Segments[3].DurationSec)
	}
	var disc, maps int
	for _, a := range m.Anchors {
		switch a.Kind {
		case AnchorDiscontinuity:
			disc++
			if a.SegmentIndex != 2 {
				t.Fatalf("disc anchor at %d", a.SegmentIndex)
			}
		case AnchorMap:
			maps++
		}
	}
	if disc != 1 || maps != 1 {
		t.Fatalf("anchors disc=%d map=%d", disc, maps)
	}
}

func TestParseHLSRejectsRemoteAndTraversal(t *testing.T) {
	cases := map[string]string{
		"remote":   "#EXTM3U\n#EXTINF:1,\nhttps://evil/x.m4s\n",
		"traversal": "#EXTM3U\n#EXT-X-MAP:URI=\"../../init.mp4\"\n#EXTINF:1,\na.m4s\n",
	}
	for name, doc := range cases {
		if _, err := ParseHLS(strings.NewReader(doc)); err == nil {
			t.Fatalf("%s: expected rejection", name)
		}
	}
}
