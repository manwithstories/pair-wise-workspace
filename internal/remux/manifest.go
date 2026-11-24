package remux

import (
	"fmt"
	"strings"

	"mediac/internal/manifest"
)

// renderHLS emits a media playlist referencing only valid fragments. When one
// or more damaged segments were removed, an EXT-X-DISCONTINUITY is inserted at
// each gap boundary so players never treat the surviving pieces as one
// continuous clock. Media sequence keeps original numbering.
func renderHLS(rep manifest.Representation, kept []keptSeg) []byte {
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:9\n")
	b.WriteString("#EXT-X-TARGETDURATION:1\n")
	if len(kept) > 0 {
		fmt.Fprintf(&b, "#EXT-X-MEDIA-SEQUENCE:%d\n", kept[0].seg.Index)
	}
	fmt.Fprintf(&b, "#EXT-X-MAP:URI=\"init.mp4\"\n")
	prevIndex := -1
	for _, k := range kept {
		if prevIndex >= 0 && k.seg.Index != prevIndex+1 {
			b.WriteString("#EXT-X-DISCONTINUITY\n") // gap: damaged range omitted
		}
		durSec := float64(k.seg.DurationMs) / 1000.0
		fmt.Fprintf(&b, "#EXTINF:%.3f,\n", durSec)
		fmt.Fprintf(&b, "seg-%d.m4s\n", k.seg.Index)
		prevIndex = k.seg.Index
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	return []byte(b.String())
}

// renderDASH emits a SegmentTemplate MPD that preserves each surviving
// fragment's original time ($Time$) and index ($Number$). Removed segments
// simply have no entry, so a consumer cannot splice across the damage.
func renderDASH(rep manifest.Representation, kept []keptSeg) []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?>` + "\n")
	b.WriteString(`<MPD type="static" profiles="urn:mpeg:dash:profile:isoff-on-demand:2011">` + "\n")
	b.WriteString("  <Period>\n    <AdaptationSet contentType=\"video\">\n")
	fmt.Fprintf(&b,
		"      <Representation id=\"%s\" codecs=\"%s\" bandwidth=\"%d\" width=\"%d\" height=\"%d\">\n",
		rep.ID, rep.Codec, rep.TargetBPS, rep.Width, rep.Height)
	fmt.Fprintf(&b,
		"        <SegmentTemplate timescale=\"1000\" initialization=\"init.mp4\" media=\"seg-$Number$.m4s\">\n")
	b.WriteString("          <SegmentTimeline>\n")
	for _, k := range kept {
		fmt.Fprintf(&b, "            <S t=\"%d\" d=\"%d\"/>\n",
			k.seg.StartMs, k.seg.DurationMs)
	}
	b.WriteString("          </SegmentTimeline>\n")
	b.WriteString("        </SegmentTemplate>\n")
	b.WriteString("      </Representation>\n    </AdaptationSet>\n  </Period>\n</MPD>\n")
	return []byte(b.String())
}
