package fixture

import (
	"fmt"
	"strings"
)

func writeDASH() string {
	var reps strings.Builder
	for _, r := range []struct {
		id, codecs string
		bw, w, h   int
	}{
		{"video-hd", "avc1.640028", 2_400_000, 1280, 720},
		{"video-ld", "avc1.64001f", 900_000, 640, 360},
	} {
		fmt.Fprintf(&reps, `
      <Representation id="%s" codecs="%s" bandwidth="%d" width="%d" height="%d">
        <SegmentTemplate timescale="44100" startNumber="1"
          initialization="init-0.mp4" media="segments/seg-$Number%%02d$.m4s">
          <SegmentTimeline><S t="0" d="23520" r="8"/></SegmentTimeline>
        </SegmentTemplate>
      </Representation>`, r.id, r.codecs, r.bw, r.w, r.h)
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static" mediaPresentationDuration="PT4.8S" profiles="urn:mpeg:dash:profile:isoff-on-demand:2011">
  <Period id="p0">
    <AdaptationSet contentType="video" mimeType="video/mp4">%s
    </AdaptationSet>
    <AdaptationSet contentType="audio" mimeType="audio/mp4">
      <Representation id="audio6" codecs="ipcm" bandwidth="1500000">
        <SegmentTemplate timescale="44100" startNumber="1"
          initialization="init-0.mp4" media="segments/seg-$Number%%02d$.m4s">
          <SegmentTimeline><S t="0" d="23520" r="8"/></SegmentTimeline>
        </SegmentTemplate>
      </Representation>
    </AdaptationSet>
  </Period>
</MPD>
`, reps.String())
}

func writeHLS(missingInit bool) string {
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:1\n")
	period := 0
	for i := 1; i <= SegmentCount; i++ {
		newPeriod := (i == 4 || i == 7)
		if newPeriod {
			period++
			b.WriteString("#EXT-X-DISCONTINUITY\n")
			if !(missingInit && period == 2) {
				fmt.Fprintf(&b, "#EXT-X-MAP:URI=\"init-%d.mp4\"\n", period)
			}
		}
		if i == 1 {
			b.WriteString("#EXT-X-MAP:URI=\"init-0.mp4\"\n")
		}
		b.WriteString("#EXTINF:0.533333333,\n")
		fmt.Fprintf(&b, "segments/seg-%02d.m4s\n", i)
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	return b.String()
}
