package manifest

import (
	"strings"
	"testing"

	"archive/internal/model"
)

func TestParseHLSCanonicalTimeline(t *testing.T) {
	src := `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:4
#EXT-X-MEDIA-SEQUENCE:10
#EXTINF:4.0,
seg10.ts
#EXT-X-DISCONTINUITY
#EXTINF:4.0,
seg11.ts
#EXTINF:2.5,
sub/seg12.ts
#EXT-X-ENDLIST
`
	tl, err := ParseHLS(strings.NewReader(src), "/archive/play", model.NewFaultLog())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tl.Format != FormatHLS || len(tl.Segments) != 3 {
		t.Fatalf("bad timeline: %+v", tl)
	}
	if tl.Segments[0].Seq != 10 || tl.Segments[2].Seq != 12 {
		t.Fatalf("sequence numbering wrong: %v", tl.Segments)
	}
	if tl.Segments[1].Start != 4*model.Second || tl.Segments[2].Start != 8*model.Second {
		t.Fatalf("starts wrong: %d %d", tl.Segments[1].Start, tl.Segments[2].Start)
	}
	if !tl.Segments[1].Discontinuity || tl.Segments[0].Discontinuity {
		t.Fatalf("discontinuity marker lost")
	}
	if tl.Segments[2].End-tl.Segments[2].Start != model.Nano(2.5*float64(model.Second)) {
		t.Fatalf("fractional duration wrong: %v", tl.Segments[2].Duration)
	}
	if got := tl.Segments[2].URI; got != "sub/seg12.ts" {
		t.Fatalf("uri not confined/cleaned: %q", got)
	}
}

func TestParseHLSRejectsBadInput(t *testing.T) {
	base := "/archive/play"
	cases := map[string]string{
		"duplicate number": "#EXT-X-MEDIA-SEQUENCE:1\n#EXTINF:2,\na.ts\n#EXT-X-MEDIA-SEQUENCE:1\n#EXTINF:2,\nb.ts\n",
		"invalid duration": "#EXTINF:0,\na.ts\n",
		"huge duration":    "#EXTINF:999,\na.ts\n",
		"path escape":      "#EXTINF:2,\n../../etc/passwd\n",
		"absolute path":    "#EXTINF:2,\n/var/secret/a.ts\n",
		"remote url":       "#EXTINF:2,\nhttps://host/a.ts\n",
		"master playlist":  "#EXT-X-STREAM-INF:BANDWIDTH=1280000\nhttp://host/high.m3u8\n",
		"empty playlist":   "#EXTM3U\n#EXT-X-VERSION:3\n",
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			log := model.NewFaultLog()
			if _, err := ParseHLS(strings.NewReader(src), base, log); err == nil {
				t.Fatalf("expected rejection for %s", name)
			}
		})
	}
}

func TestParseDASHTemplate(t *testing.T) {
	src := `<?xml version="1.0"?>
<MPD type="static" mediaPresentationDuration="PT8S">
  <Period duration="PT8S">
    <SegmentTemplate timescale="1000" media="chunk-$Number$.m4s" startNumber="3" duration="2000"/>
  </Period>
</MPD>`
	tl, err := ParseDASH(strings.NewReader(src), "/archive/mpd", model.NewFaultLog())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tl.Segments) != 4 {
		t.Fatalf("want 4 expanded segments, got %d", len(tl.Segments))
	}
	if tl.Segments[0].Seq != 3 || tl.Segments[3].Seq != 6 {
		t.Fatalf("startNumber expansion wrong: %v", tl.Segments)
	}
	for _, s := range tl.Segments {
		if s.Duration != 2*model.Second {
			t.Fatalf("timescale conversion wrong: %v", s.Duration)
		}
	}
	if tl.End() != 8*model.Second {
		t.Fatalf("timeline end wrong: %v", tl.End())
	}
}

func TestParseDASHSegmentList(t *testing.T) {
	src := `<MPD>
  <Period duration="PT4S">
    <SegmentList duration="2000" timescale="1000">
      <Segment media="a.m4s"/>
      <Segment media="b.m4s" discontinuity="true"/>
    </SegmentList>
  </Period>
</MPD>`
	tl, err := ParseDASH(strings.NewReader(src), "/archive/mpd", model.NewFaultLog())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tl.Segments) != 2 || !tl.Segments[1].Discontinuity {
		t.Fatalf("segment list parse wrong: %+v", tl.Segments)
	}
}

func TestParseDASHRejectsRemoteAndBad(t *testing.T) {
	log := model.NewFaultLog()
	if _, err := ParseDASH(strings.NewReader(`<MPD><BaseURL sourceURL="https://cdn/x/"/></MPD>`),
		"/a", log); err == nil {
		t.Fatal("remote BaseURL must be rejected")
	}
	if log.Count(model.FaultPathRejected) == 0 {
		t.Fatal("expected a path_rejected fault")
	}
	if _, err := ParseDASH(strings.NewReader(`<MPD><Period duration="PT4S"><SegmentURL media="../x.m4s" duration="0"/></Period></MPD>`),
		"/a", model.NewFaultLog()); err == nil {
		t.Fatal("escaping path / zero duration must be rejected")
	}
}

func TestParseISO8601(t *testing.T) {
	cases := map[string]float64{
		"PT4S":    4,
		"PT1.5S":  1.5,
		"PT1M30S": 90,
		"P1DT12H": 129600,
	}
	for in, want := range cases {
		got, ok := parseISO8601(in)
		if !ok || got != want {
			t.Fatalf("parseISO8601(%q) = %v,%v want %v", in, got, ok, want)
		}
	}
	if _, ok := parseISO8601("garbage"); ok {
		t.Fatal("garbage duration must fail")
	}
}
