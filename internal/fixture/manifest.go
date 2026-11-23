package fixture

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// HLSSegments renders a media playlist for the given segments and camera.
// The hash of every payload is emitted as EXT-X-MEDIA-HASH ahead of its URI
// so the parser can bind content commitments to entries.
func HLSSegments(camera string, segs []Segment) string {
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:5\n#EXT-X-MEDIA-SEQUENCE:1\n")
	for _, s := range segs {
		uri := fmt.Sprintf("%s/%06d.m4s", camera, s.Seq)
		h := sha256.Sum256(s.Payload)
		fmt.Fprintf(&b, "#EXT-X-MEDIA-HASH:%s\n", hex.EncodeToString(h[:]))
		fmt.Fprintf(&b, "#EXTINF:%.3f,\n%s\n", float64(s.DurNS)/1e9, uri)
	}
	return b.String()
}

// DASHSegments renders a SegmentTimeline MPD matching the same segments.
func DASHSegments(camera string, segs []Segment, timescale uint64) string {
	durTicks := uint64(segs[0].DurNS) * timescale / 1_000_000_000
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?>` + "\n")
	b.WriteString(`<MPD type="static">` + "\n")
	fmt.Fprintf(&b, "  <BaseURL>%s/</BaseURL>\n", camera)
	b.WriteString(`  <Period id="p0">` + "\n")
	fmt.Fprintf(&b, "    <AdaptationSet id=\"v\" mimeType=\"video/mp4\" timescale=\"%d\">\n", timescale)
	fmt.Fprintf(&b, "      <Representation id=\"r0\" bandwidth=\"800000\">\n")
	fmt.Fprintf(&b, "        <SegmentTemplate media=\"$Number%%06d$.m4s\" startNumber=\"1\" timescale=\"%d\"/>\n", timescale)
	b.WriteString("        <SegmentTimeline>\n")
	fmt.Fprintf(&b, "          <S t=\"0\" d=\"%d\" r=\"%d\"/>\n", durTicks, len(segs)-1)
	b.WriteString("        </SegmentTimeline>\n")
	b.WriteString("      </Representation>\n    </AdaptationSet>\n  </Period>\n</MPD>\n")
	return b.String()
}
