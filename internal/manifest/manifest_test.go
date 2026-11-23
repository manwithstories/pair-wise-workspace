package manifest

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

const hlsMedia = `#EXTM3U
#EXT-X-VERSION:7
#EXT-X-TARGETDURATION:2
#EXT-X-MEDIA-SEQUENCE:10
#EXT-X-MAP:URI="init.mp4"
#EXTINF:2.000,
seg10.m4s
#EXTINF:2.000,
seg11.m4s
#EXT-X-DISCONTINUITY
#EXTINF:2.000,
seg12.m4s
#EXT-X-ENDLIST
`

func TestParseHLSMedia(t *testing.T) {
	g, err := Parse([]byte(hlsMedia), nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if g.Format != FormatHLS || len(g.Plans) != 1 {
		t.Fatalf("group=%+v", g)
	}
	p := g.Plans[0]
	if len(p.Segments) != 3 {
		t.Fatalf("segments=%d", len(p.Segments))
	}
	if p.Segments[0].Sequence != 10 || p.Segments[2].Sequence != 12 {
		t.Fatalf("sequence numbering wrong: %+v", p.Segments)
	}
	if p.Segments[1].StartNs != 2_000_000_000 {
		t.Fatalf("start=%d", p.Segments[1].StartNs)
	}
	if !p.Segments[2].Discontinuity || p.Discontinuities != 1 {
		t.Fatalf("discontinuity not tracked: %+v", p)
	}
	if p.Segments[0].InitURI != "init.mp4" {
		t.Fatalf("init uri=%q", p.Segments[0].InitURI)
	}
	if p.DurationNs != 6_000_000_000 {
		t.Fatalf("duration=%d", p.DurationNs)
	}
	if len(p.Digest) != 64 {
		t.Fatalf("digest len %d", len(p.Digest))
	}
}

const hlsMaster = `#EXTM3U
#EXT-X-STREAM-INF:BANDWIDTH=800000,CODECS="avc1.64001f,mp4a.40.2"
video_800.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=1600000,CODECS="avc1.640028,mp4a.40.2"
video_1600.m3u8
`

func TestParseHLSMasterExpandsVariants(t *testing.T) {
	child := strings.ReplaceAll(hlsMedia, "seg", "v")
	load := func(uri string) ([]byte, error) { return []byte(child), nil }
	g, err := Parse([]byte(hlsMaster), load, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Plans) != 2 {
		t.Fatalf("plans=%d", len(g.Plans))
	}
	if g.Plans[0].Bandwidth != 800000 || g.Plans[0].Representation != "video_800.m3u8" {
		t.Fatalf("plan0=%+v", g.Plans[0])
	}
}

func TestHLSRejectsTraversal(t *testing.T) {
	for _, uri := range []string{
		"../escape.m4s",
		"a/../../escape.m4s",
		"%2e%2e/escape.m4s",
		"/etc/passwd",
		"http://evil.example/x.m4s",
		"..\\escape.m4s",
	} {
		doc := "#EXTM3U\n#EXTINF:2,\n" + uri + "\n"
		_, err := Parse([]byte(doc), nil, Options{})
		if !errors.Is(err, ErrPathTraversal) && !errors.Is(err, ErrInvalid) {
			t.Errorf("%q: err=%v, want traversal/invalid", uri, err)
		}
	}
}

func TestHLSRejectsDuplicateURI(t *testing.T) {
	doc := "#EXTM3U\n#EXTINF:2,\na.m4s\n#EXTINF:2,\na.m4s\n"
	_, err := Parse([]byte(doc), nil, Options{})
	if !errors.Is(err, ErrDuplicateURI) {
		t.Fatalf("err=%v want duplicate", err)
	}
}

func TestHLSRejectsOversize(t *testing.T) {
	doc := "#EXTM3U\n"
	for i := 0; i < 20; i++ {
		doc += fmt.Sprintf("#EXTINF:2,\nseg%d.m4s\n", i)
	}
	if _, err := Parse([]byte(doc), nil, Options{MaxSegments: 5}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err=%v want too large", err)
	}
	if _, err := Parse([]byte(doc), nil, Options{MaxBytes: 32}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err=%v want too large by bytes", err)
	}
}

const dashDoc = `<?xml version="1.0"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static" mediaPresentationDuration="PT6S">
  <Period id="p0">
    <AdaptationSet mimeType="video/mp4" codecs="avc1.64001f">
      <SegmentTemplate timescale="1000" duration="2000" startNumber="5"
        initialization="init-$RepresentationID$.mp4"
        media="seg-$RepresentationID$-$Number%05d$.m4s"/>
      <Representation id="v720" bandwidth="900000" width="1280" height="720"/>
      <Representation id="v1080" bandwidth="2400000" width="1920" height="1080"/>
    </AdaptationSet>
  </Period>
</MPD>`

func TestParseDASHMultiRepresentation(t *testing.T) {
	g, err := Parse([]byte(dashDoc), nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if g.Format != FormatDASH || len(g.Plans) != 2 {
		t.Fatalf("group=%+v", g)
	}
	// Two representations with different bandwidths must not fail the
	// co-duration check.
	for _, p := range g.Plans {
		if len(p.Segments) != 3 {
			t.Fatalf("%s segments=%d", p.Representation, len(p.Segments))
		}
		if p.Timescale != 1000 {
			t.Fatalf("timescale=%d", p.Timescale)
		}
		if p.Segments[0].DurationNs != 2_000_000_000 {
			t.Fatalf("duration=%d", p.Segments[0].DurationNs)
		}
	}
	p := g.Plans[0]
	if p.Segments[0].URI != "seg-v720-00005.m4s" {
		t.Fatalf("template expansion: %q", p.Segments[0].URI)
	}
	if p.Segments[0].InitURI != "init-v720.mp4" {
		t.Fatalf("init=%q", p.Segments[0].InitURI)
	}
	if p.Segments[2].URI != "seg-v720-00007.m4s" {
		t.Fatalf("seq numbering: %q", p.Segments[2].URI)
	}
}

func TestDASHSegmentList(t *testing.T) {
	doc := `<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" mediaPresentationDuration="PT4S">
  <Period id="p0">
    <AdaptationSet mimeType="video/mp4">
      <Representation id="v" bandwidth="1000">
        <SegmentList timescale="1000" duration="2000">
          <Initialization sourceURL="init.mp4"/>
          <SegmentURL media="a.m4s" mediaRange="0-999"/>
          <SegmentURL media="b.m4s"/>
        </SegmentList>
      </Representation>
    </AdaptationSet>
  </Period>
</MPD>`
	g, err := Parse([]byte(doc), nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	p := g.Plans[0]
	if len(p.Segments) != 2 {
		t.Fatalf("segments=%d", len(p.Segments))
	}
	if p.Segments[0].ByteRange == nil || p.Segments[0].ByteRange.Length != 1000 {
		t.Fatalf("byte range=%+v", p.Segments[0].ByteRange)
	}
	if p.Segments[0].InitURI != "init.mp4" {
		t.Fatalf("init=%q", p.Segments[0].InitURI)
	}
}

func TestDASHDurationParsing(t *testing.T) {
	cases := map[string]int64{
		"PT1S":    1_000_000_000,
		"PT1M30S": 90_000_000_000,
		"PT1H":    3600_000_000_000,
		"P1DT1S":  86_401_000_000_000,
		"PT0.5S":  500_000_000,
	}
	for in, want := range cases {
		got, err := parseISO8601Duration(in)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if got != want {
			t.Errorf("%s=%d want %d", in, got, want)
		}
	}
	if _, err := parseISO8601Duration("bogus"); err == nil {
		t.Fatal("expected error for bogus duration")
	}
}

func TestDASHRejectsTraversalAndOversize(t *testing.T) {
	bad := strings.Replace(dashDoc, `media="seg-$RepresentationID$-$Number%05d$.m4s"`, `media="../seg-$Number$.m4s"`, 1)
	if _, err := Parse([]byte(bad), nil, Options{}); !errors.Is(err, ErrPathTraversal) {
		t.Fatalf("err=%v want traversal", err)
	}
	if _, err := Parse([]byte(dashDoc), nil, Options{MaxSegments: 1}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err=%v want too large", err)
	}
}

func TestManifestDeterministicDigest(t *testing.T) {
	a, err := Parse([]byte(dashDoc), nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := Parse([]byte(dashDoc), nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if a.Digest != b.Digest || len(a.Digest) != 64 {
		t.Fatalf("digest %q vs %q", a.Digest, b.Digest)
	}
}
