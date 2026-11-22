package manifest

import (
	"fmt"
	"strings"
	"testing"
)

func hlsPlaylist(n int) string {
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:2\n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "#EXTINF:2.000000,\nseg-%05d.m4s\n", i)
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	return b.String()
}

func TestHLSOK(t *testing.T) {
	segs, err := ParseHLS([]byte(hlsPlaylist(4)), "media.m3u8", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 4 {
		t.Fatalf("got %d segments", len(segs))
	}
	if segs[2].URI != "seg-00002.m4s" || segs[2].Source != "media.m3u8" {
		t.Fatalf("bad provenance: %+v", segs[2])
	}
	if segs[3].Start.Float64() != 6.0 {
		t.Fatalf("segment 3 start = %v, want 6s", segs[3].Start)
	}
}

func TestHLSRejects(t *testing.T) {
	cases := map[string]string{
		"no header":   "#EXT-X-TARGETDURATION:2\n#EXTINF:2,\na.m4s\n",
		"negative":    "#EXTM3U\n#EXTINF:-1.5,\na.m4s\n",
		"escape":      "#EXTM3U\n#EXTINF:2,\n../../etc/passwd\n",
		"absolute":    "#EXTM3U\n#EXTINF:2,\n/tmp/a.m4s\n",
		"url":         "#EXTM3U\n#EXTINF:2,\nhttps://evil.example/a.m4s\n",
		"backslash":   "#EXTM3U\n#EXTINF:2,\n..\\a.m4s\n",
		"overflow":    "#EXTM3U\n#EXTINF:99999999999999999999,\na.m4s\n",
		"huge target": "#EXTM3U\n#EXT-X-TARGETDURATION:99999999\n#EXTINF:2,\na.m4s\n",
		"no extinf":   "#EXTM3U\na.m4s\n",
		"empty":       "#EXTM3U\n#EXT-X-ENDLIST\n",
	}
	for name, pl := range cases {
		if _, err := ParseHLS([]byte(pl), "m.m3u8", ""); err == nil {
			t.Errorf("%s: expected rejection", name)
		}
	}
}

func TestHLSDuplicatePreserved(t *testing.T) {
	pl := "#EXTM3U\n#EXTINF:2,\na.m4s\n#EXTINF:2,\na.m4s\n#EXT-X-ENDLIST\n"
	segs, err := ParseHLS([]byte(pl), "m.m3u8", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 2 || segs[0].URI != segs[1].URI {
		t.Fatalf("duplicates must be preserved for pipeline dedupe: %+v", segs)
	}
}

const dashMPDOK = `<MPD type="static"><Period><AdaptationSet mimeType="video/mp4">
<SegmentTemplate timescale="90000" media="seg-$Number%05d$.m4s" startNumber="0">
<SegmentTimeline><S t="0" d="180000" r="2"/></SegmentTimeline>
</SegmentTemplate></AdaptationSet></Period></MPD>`

func TestDASHOK(t *testing.T) {
	segs, err := ParseDASH([]byte(dashMPDOK), "manifest.mpd", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 3 {
		t.Fatalf("got %d segments", len(segs))
	}
	if segs[1].URI != "seg-00001.m4s" {
		t.Fatalf("bad template expansion: %s", segs[1].URI)
	}
	if segs[2].Start.Float64() != 4.0 {
		t.Fatalf("segment 2 start = %v, want 4s", segs[2].Start)
	}
}

func TestDASHTwoRuns(t *testing.T) {
	mpd := `<MPD><Period><AdaptationSet>
<SegmentTemplate timescale="1000" media="s$Number$.m4s">
<SegmentTimeline><S t="0" d="2000" r="1"/><S d="1500" r="0"/></SegmentTimeline>
</SegmentTemplate></AdaptationSet></Period></MPD>`
	segs, err := ParseDASH([]byte(mpd), "m.mpd", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 3 || segs[2].Start.Float64() != 4.0 || segs[2].Dur.Float64() != 1.5 {
		t.Fatalf("bad chained timeline: %+v", segs)
	}
}

func TestDASHRejects(t *testing.T) {
	cases := map[string]string{
		"negative d": `<MPD><Period><AdaptationSet><SegmentTemplate timescale="1000" media="s$Number$.m4s"><SegmentTimeline><S t="0" d="-5" r="1"/></SegmentTimeline></SegmentTemplate></AdaptationSet></Period></MPD>`,
		"bad repeat": `<MPD><Period><AdaptationSet><SegmentTemplate timescale="1000" media="s$Number$.m4s"><SegmentTimeline><S t="0" d="5" r="-2"/></SegmentTimeline></SegmentTemplate></AdaptationSet></Period></MPD>`,
		"overflow":   `<MPD><Period><AdaptationSet><SegmentTemplate timescale="1000" media="s$Number$.m4s"><SegmentTimeline><S t="4611686018427387900" d="4611686018427387900" r="2"/></SegmentTimeline></SegmentTemplate></AdaptationSet></Period></MPD>`,
		"escape":     `<MPD><Period><AdaptationSet><SegmentTemplate timescale="1000" media="../s$Number$.m4s"><SegmentTimeline><S t="0" d="5" r="1"/></SegmentTimeline></SegmentTemplate></AdaptationSet></Period></MPD>`,
		"no timescale": `<MPD><Period><AdaptationSet><SegmentTemplate media="s$Number$.m4s"><SegmentTimeline><S t="0" d="5" r="1"/></SegmentTimeline></SegmentTemplate></AdaptationSet></Period></MPD>`,
		"no timeline":  `<MPD><Period><AdaptationSet><SegmentTemplate timescale="1000" duration="5" media="s$Number$.m4s"/></AdaptationSet></Period></MPD>`,
	}
	for name, mpd := range cases {
		if _, err := ParseDASH([]byte(mpd), "m.mpd", ""); err == nil {
			t.Errorf("%s: expected rejection", name)
		}
	}
}

func TestDASHTimeTemplate(t *testing.T) {
	mpd := `<MPD><Period><AdaptationSet><SegmentTemplate timescale="1000" media="chunk-$Time$.m4s"><SegmentTimeline><S t="100" d="50" r="1"/></SegmentTimeline></SegmentTemplate></AdaptationSet></Period></MPD>`
	segs, err := ParseDASH([]byte(mpd), "m.mpd", "")
	if err != nil {
		t.Fatal(err)
	}
	if segs[0].URI != "chunk-100.m4s" || segs[1].URI != "chunk-150.m4s" {
		t.Fatalf("bad $Time$ expansion: %+v", segs)
	}
}
