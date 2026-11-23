package manifest

import (
	"strings"
	"testing"

	"medrelay/internal/timeline"
)

const goodHLS = `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:6
#EXT-X-MEDIA-SEQUENCE:0
#EXTINF:6.000,
seg-0.m4s
#EXTINF:6.000,
seg-1.m4s
#EXTINF:5.500,
seg-2.m4s
#EXT-X-ENDLIST
`

func TestParseHLSGood(t *testing.T) {
	segs, err := ParseHLS(strings.NewReader(goodHLS))
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 3 {
		t.Fatalf("got %d segments, want 3", len(segs))
	}
	if segs[0].StartMs != 0 || segs[1].StartMs != 6000 || segs[2].StartMs != 12000 {
		t.Fatalf("bad starts: %+v", segs)
	}
	if segs[2].Seq != 2 {
		t.Fatalf("bad seq: %+v", segs[2])
	}
}

func TestParseHLSDuplicateSeq(t *testing.T) {
	bad := strings.Replace(goodHLS, "seg-1.m4s", "seg-0.m4s", 1)
	_, err := ParseHLS(strings.NewReader(bad))
	te, ok := err.(*timeline.Error)
	if !ok || te.Code != timeline.ErrDuplicateSeq {
		t.Fatalf("want duplicate_sequence, got %v", err)
	}
	if te.Segment != "seg-0.m4s" {
		t.Fatalf("error not located: %v", te)
	}
}

func TestParseHLSInvalidDuration(t *testing.T) {
	bad := strings.Replace(goodHLS, "#EXTINF:6.000,", "#EXTINF:-1.5,", 1)
	_, err := ParseHLS(strings.NewReader(bad))
	if te, ok := err.(*timeline.Error); !ok || te.Code != timeline.ErrInvalidDur {
		t.Fatalf("want invalid_duration, got %v", err)
	}
}

func TestParseHLSMissingRef(t *testing.T) {
	bad := "#EXTM3U\n#EXTINF:6.000,\n#EXTINF:6.000,\nseg-0.m4s\n"
	_, err := ParseHLS(strings.NewReader(bad))
	if te, ok := err.(*timeline.Error); !ok || te.Code != timeline.ErrMissingRef {
		t.Fatalf("want missing_reference, got %v", err)
	}
}

const goodDASH = `<?xml version="1.0"?>
<MPD><Period><AdaptationSet>
<Representation id="v1">
<SegmentTemplate timescale="90000" media="seg-$Number$.m4s" startNumber="0">
<SegmentTimeline>
<S t="0" d="540000" r="2"/>
</SegmentTimeline>
</SegmentTemplate>
</Representation>
</AdaptationSet></Period></MPD>
`

func TestParseDASHGood(t *testing.T) {
	tls, err := ParseDASH(strings.NewReader(goodDASH))
	if err != nil {
		t.Fatal(err)
	}
	if len(tls) != 1 || len(tls[0].Segments) != 3 {
		t.Fatalf("got %+v", tls)
	}
	s := tls[0].Segments
	if s[0].URI != "seg-0.m4s" || s[1].StartMs != 6000 || s[2].StartMs != 12000 {
		t.Fatalf("bad expansion: %+v", s)
	}
}

func TestParseDASHOverlap(t *testing.T) {
	bad := strings.Replace(goodDASH, `<S t="0" d="540000" r="2"/>`,
		`<S t="0" d="540000"/><S t="1" d="540000"/>`, 1)
	_, err := ParseDASH(strings.NewReader(bad))
	if te, ok := err.(*timeline.Error); !ok || te.Code != timeline.ErrDuplicateSeq {
		t.Fatalf("want duplicate_sequence (overlap), got %v", err)
	}
}

func TestParseDASHMissingMedia(t *testing.T) {
	bad := strings.Replace(goodDASH, ` media="seg-$Number$.m4s"`, "", 1)
	_, err := ParseDASH(strings.NewReader(bad))
	if te, ok := err.(*timeline.Error); !ok || te.Code != timeline.ErrMissingRef {
		t.Fatalf("want missing_reference, got %v", err)
	}
}

func TestParseDASHBadDuration(t *testing.T) {
	bad := strings.Replace(goodDASH, `d="540000" r="2"`, `d="0" r="2"`, 1)
	_, err := ParseDASH(strings.NewReader(bad))
	if te, ok := err.(*timeline.Error); !ok || te.Code != timeline.ErrInvalidDur {
		t.Fatalf("want invalid_duration, got %v", err)
	}
}
