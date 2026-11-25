package manifest

import (
	"strings"
	"testing"

	"fieldpost/internal/core"
)

const hlsMedia = `#EXTM3U
#EXT-X-VERSION:7
#EXT-X-TARGETDURATION:2
#EXT-X-MEDIA-SEQUENCE:10
#EXTINF:2.000,
cam_a_10.ts
#EXTINF:1.500,
cam_a_11.ts
`

const hlsMaster = `#EXTM3U
#EXT-X-STREAM-INF:BANDWIDTH=5000000,RESOLUTION=1920x1080
video.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=128000
audio.m3u8
`

func TestParseHLSMasterAndMedia(t *testing.T) {
	master, err := ParseHLS("master.m3u8", []byte(hlsMaster))
	if err != nil {
		t.Fatal(err)
	}
	if master.Kind != KindHLSMaster || len(master.Variants) != 2 {
		t.Fatalf("master=%+v", master)
	}
	m, err := ParseHLS("media.m3u8", []byte(hlsMedia))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Segments) != 2 || m.Segments[0].Number != 10 || m.Segments[1].Number != 11 {
		t.Fatalf("segments=%+v", m.Segments)
	}
	if m.Segments[0].Duration != 2_000_000_000 || m.Segments[1].Duration != 1_500_000_000 {
		t.Fatalf("durations wrong: %+v", m.Segments)
	}
	if m.Segments[0].Offset != 5 { // #EXTINF line
		t.Fatalf("line offset=%d", m.Segments[0].Offset)
	}
}

func TestHLSRejectsContractViolations(t *testing.T) {
	cases := map[string]string{
		"missing header":    "#EXTINF:1.0,\nx.ts\n",
		"negative duration": "#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXTINF:-1.0,\nx.ts\n",
		"missing EXTINF":    "#EXTM3U\n#EXT-X-TARGETDURATION:2\nx.ts\n",
		"missing target":    "#EXTM3U\n#EXTINF:1.0,\nx.ts\n",
		"path traversal":    "#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXTINF:1.0,\n../../x.ts\n",
		"absolute uri":      "#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXTINF:1.0,\n/etc/x.ts\n",
		"external scheme":   "#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXTINF:1.0,\nhttps://x/a.ts\n",
		"duplicate number":  "#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXT-X-SEGMENT-NUMBER:7\n#EXTINF:1.0,\na.ts\n#EXT-X-SEGMENT-NUMBER:7\n#EXTINF:1.0,\nb.ts\n",
	}
	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseHLS(name+".m3u8", []byte(text))
			if err == nil {
				t.Fatal("expected rejection")
			}
			if core.ExitCode(err) != core.ExitUsage {
				t.Fatalf("exit=%d err=%v", core.ExitCode(err), err)
			}
			fe, ok := core.AsError(err)
			if !ok || fe.Stage != core.StageManifest || fe.Offset == 0 && name != "missing target" {
				t.Fatalf("error lacks structured location: %#v", err)
			}
		})
	}
}

const dashTemplate = `<?xml version="1.0"?>
<MPD mediaPresentationDuration="PT8S" minBufferTime="PT2S">
 <Period>
  <Representation id="v0" codecs="avc1.640028" bandwidth="5000000">
   <SegmentTemplate timescale="90000" startNumber="4" media="chunk-$Number$.m4s" initialization="init.mp4">
    <SegmentTimeline>
     <S d="180000" r="3"/>
    </SegmentTimeline>
   </SegmentTemplate>
  </Representation>
 </Period>
</MPD>`

const dashList = `<MPD><Period><AdaptationSet><Representation id="a0" codecs="mp4a.40.2">
 <SegmentList timescale="48000" duration="48000">
  <SegmentURL media="a-1.m4a"/>
  <SegmentURL media="a-2.m4a"/>
 </SegmentList>
</Representation></AdaptationSet></Period></MPD>`

func TestParseDASHTemplateAndList(t *testing.T) {
	m, err := ParseDASH("index.mpd", []byte(dashTemplate))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Segments) != 4 || m.Segments[0].Number != 4 || m.Segments[3].URI != "chunk-7.m4s" {
		t.Fatalf("template segments=%+v", m.Segments)
	}
	if m.Segments[0].Duration != 2_000_000_000 {
		t.Fatalf("duration=%d", m.Segments[0].Duration)
	}
	lm, err := ParseDASH("list.mpd", []byte(dashList))
	if err != nil {
		t.Fatal(err)
	}
	if len(lm.DASH.Representations[0].Segments) != 2 {
		t.Fatalf("list=%+v", lm.DASH.Representations)
	}
}

func TestDASHRejectsBadStaticMPD(t *testing.T) {
	bad := []string{
		`<MPD><Representation id="v"><SegmentTemplate startNumber="1"/></Representation></MPD>`,
		`<MPD><Representation id="v"><SegmentList><SegmentURL number="2" media="a.m4s"/><SegmentURL number="2" media="b.m4s"/></SegmentList></Representation></MPD>`,
		`<MPD><Representation id="v"><SegmentList><SegmentURL media="../../a.m4s"/></SegmentList></Representation></MPD>`,
		`<MPD mediaPresentationDuration="-PT1S"><Representation id="v"/></MPD>`,
		`<MPD><Representation id="v"><SegmentTemplate timescale="90000" media="$Number$.m4s"><SegmentTimeline><S d="-3"/></SegmentTimeline></SegmentTemplate></Representation></MPD>`,
		`<MPD><Representation id="v"><SegmentList/></Representation></MPD>`,
	}
	rejected := 0
	for i, xml := range bad {
		_, err := ParseDASH("bad.mpd", []byte(xml))
		if err == nil {
			t.Fatalf("case %d accepted", i)
		}
		if !strings.Contains(err.Error(), "manifest") {
			t.Fatalf("case %d wrong stage: %v", i, err)
		}
		rejected++
	}
	if rejected < 4 {
		t.Fatalf("rejected=%d", rejected)
	}
}
