package fixture

import (
	"fmt"
	"strings"
)

// HLSCameraManifest generates a valid media playlist for four shards.
func HLSCameraManifest(camera, container string) []byte {
	ext := "ts"
	if container == "mp4" {
		ext = "m4s"
	}
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:7\n")
	b.WriteString("#EXT-X-TARGETDURATION:2\n")
	b.WriteString(fmt.Sprintf("#EXT-X-MEDIA-SEQUENCE:%d\n", 40))
	if container == "mp4" {
		b.WriteString("#EXT-X-MAP:URI=\"init.mp4\"\n")
	}
	for i := 0; i < SegmentsPerCamera; i++ {
		b.WriteString("#EXTINF:2.000,\n")
		b.WriteString(fmt.Sprintf("%s-%02d.%s\n", camera, i, ext))
	}
	return []byte(b.String())
}

// HLSMaster generates a master playlist referencing three camera playlists.
func HLSMaster() []byte {
	return []byte(`#EXTM3U
#EXT-X-VERSION:7
#EXT-X-STREAM-INF:BANDWIDTH=5200000,RESOLUTION=1280x720
stage.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=4100000,RESOLUTION=1280x720
side.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=3600000,RESOLUTION=1280x720
roving.m3u8
`)
}

// DASHManifest generates a static SegmentTemplate MPD for four shards.
func DASHManifest() []byte {
	return []byte(`<?xml version="1.0" encoding="UTF-8"?>
<MPD type="static" mediaPresentationDuration="PT8S" minBufferTime="PT2S" profiles="urn:mpeg:dash:profile:isoff-on-demand:2011">
 <Period>
  <AdaptationSet contentType="video">
   <Representation id="roving-v" codecs="avc1.64001f" mimeType="video/mp4" bandwidth="3600000">
    <SegmentTemplate timescale="90000" startNumber="0" initialization="roving-init.mp4" media="roving-$Number$.m4s">
     <SegmentTimeline><S d="180000" r="3"/></SegmentTimeline>
    </SegmentTemplate>
   </Representation>
  </AdaptationSet>
  <AdaptationSet contentType="audio">
   <Representation id="roving-a" codecs="mp4a.40.2" mimeType="audio/mp4" bandwidth="128000">
    <SegmentTemplate timescale="90000" startNumber="0" initialization="roving-a-init.mp4" media="roving-a-$Number$.m4s">
     <SegmentTimeline><S d="180000" r="3"/></SegmentTimeline>
    </SegmentTemplate>
   </Representation>
  </AdaptationSet>
 </Period>
</MPD>`)
}

// BoundaryCase is a deliberately invalid input used by demo rejection counts.
type BoundaryCase struct {
	Name       string
	Kind       string
	Source     string
	HLS        string
	DASH       string
	BoxBytes   []byte
	ExpectExit int
}

// BoundaryCases returns deterministic invalid duration, URI and box fixtures.
func BoundaryCases() []BoundaryCase {
	return []BoundaryCase{
		{
			Name: "negative HLS duration", Kind: "hls", Source: "bad-duration.m3u8", ExpectExit: 2,
			HLS: "#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXTINF:-0.500,\nbad.ts\n",
		},
		{
			Name: "out-of-bounds HLS URI", Kind: "hls", Source: "bad-uri.m3u8", ExpectExit: 2,
			HLS: "#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXTINF:1.000,\nhttps://outside.example/x.ts\n",
		},
		{
			Name: "path traversal DASH URI", Kind: "dash", Source: "bad-path.mpd", ExpectExit: 2,
			DASH: `<MPD><Period><Representation id="v"><SegmentList><SegmentURL media="../../escape.m4s"/></SegmentList></Representation></Period></MPD>`,
		},
		{
			Name: "duplicate DASH number", Kind: "dash", Source: "dup.mpd", ExpectExit: 2,
			DASH: `<MPD><Period><Representation id="v"><SegmentList><SegmentURL number="1" media="a.m4s"/><SegmentURL number="1" media="b.m4s"/></SegmentList></Representation></Period></MPD>`,
		},
		{
			Name: "illegal MP4 box length", Kind: "box", Source: "illegal-box.m4s", ExpectExit: 3,
			BoxBytes: []byte{0x00, 0x00, 0x00, 0x04, 'f', 'r', 'e', 'e', 0x00},
		},
		{
			Name: "truncated MP4 box", Kind: "box", Source: "truncated-box.m4s", ExpectExit: 3,
			BoxBytes: []byte{0x00, 0x00, 0x00, 0x20, 'm', 'o', 'o', 'f', 0x00, 0x00},
		},
	}
}
