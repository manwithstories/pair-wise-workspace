package manifest

import (
	"strings"
	"testing"

	"archive/internal/errs"
)

const hlsMedia = `#EXTM3U
#EXT-X-TARGETDURATION:6
#EXT-X-MEDIA-SEQUENCE:10
#EXTINF:6.0,
seg_0010.m4s
#EXTINF:5.5,
seg_0011.m4s
`

func TestHLSMedia(t *testing.T) {
	m, err := Parse("a", []byte(hlsMedia))
	if err != nil {
		t.Fatal(err)
	}
	if m.Type != TypeHLSMedia || len(m.Segments) != 2 {
		t.Fatalf("got %+v", m)
	}
	if m.Segments[0].Index != 10 || m.Segments[1].URI != "seg_0011.m4s" {
		t.Fatalf("bad refs %+v", m.Segments)
	}
}

func TestHLSMaster(t *testing.T) {
	m, err := Parse("a", []byte(`#EXTM3U
#EXT-X-STREAM-INF:BANDWIDTH=800000
v/prog.m3u8
`))
	if err != nil || m.Type != TypeHLSMaster || len(m.Variants) != 1 {
		t.Fatalf("got %+v err %v", m, err)
	}
	if m.Variants[0].URI != "v/prog.m3u8" || m.Variants[0].Bandwidth != 800000 {
		t.Fatalf("bad variant %+v", m.Variants[0])
	}
}

func TestHLSRejectsBadInput(t *testing.T) {
	cases := []string{
		"#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXTINF:0,\ns.m4s\n",      // zero duration
		"#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXTINF:1,\n../evil.m4s\n", // traversal
		"#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXTINF:1,\n/abs.m4s\n",    // absolute
		"#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXTINF:1,\nhttp://x/s.m4s\n", // remote
		"#EXTM3U\n#EXT-X-TARGETDURATION:6\n",                          // no segments
	}
	for i, c := range cases {
		if _, err := Parse("a", []byte(c)); err == nil {
			t.Fatalf("case %d accepted", i)
		}
	}
}

const dashList = `<?xml version="1.0"?>
<MPD><Period><AdaptationSet contentType="video">
<Representation><SegmentList timescale="90000" duration="180000">
<SegmentURL media="s1.m4s"/><SegmentURL media="s2.m4s"/>
</SegmentList></Representation>
</AdaptationSet></Period></MPD>`

func TestDASHSegmentList(t *testing.T) {
	m, err := Parse("a", []byte(dashList))
	if err != nil {
		t.Fatal(err)
	}
	if m.Timescale != 90000 || len(m.Segments) != 2 || m.Segments[0].Duration != 2.0 {
		t.Fatalf("got %+v", m)
	}
}

const dashTpl = `<MPD><Period><AdaptationSet>
<SegmentTemplate media="seg_$Number%05d$.m4s" timescale="90000" startNumber="7">
<SegmentTimeline><S d="90000" r="2"/></SegmentTimeline>
</SegmentTemplate>
</AdaptationSet></Period></MPD>`

func TestDASHSegmentTemplate(t *testing.T) {
	m, err := Parse("a", []byte(dashTpl))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Segments) != 3 || m.Segments[0].URI != "seg_00007.m4s" || m.Segments[2].URI != "seg_00009.m4s" {
		t.Fatalf("got %+v", m.Segments)
	}
}

func TestDASHInconsistentTimescale(t *testing.T) {
	bad := `<MPD><Period>
<AdaptationSet><SegmentTemplate media="a$Number$.m4s" timescale="90000"><SegmentTimeline><S d="1"/></SegmentTimeline></SegmentTemplate></AdaptationSet>
<AdaptationSet><SegmentTemplate media="b$Number$.m4s" timescale="48000"><SegmentTimeline><S d="1"/></SegmentTimeline></SegmentTemplate></AdaptationSet>
</Period></MPD>`
	_, err := Parse("a", []byte(bad))
	if err == nil || !strings.Contains(err.Error(), "inconsistent timescale") {
		t.Fatalf("got %v", err)
	}
	if e, ok := err.(*errs.Error); !ok || e.Kind != errs.KindManifest {
		t.Fatalf("wrong error kind: %v", err)
	}
}
