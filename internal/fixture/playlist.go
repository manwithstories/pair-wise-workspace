package fixture

import (
	"fmt"
	"strings"

	"archive/internal/manifest"
)

// PlaylistSpec describes the local playlist material generated for one case.
type PlaylistSpec struct {
	Format          manifest.Format
	SegmentDuration float64 // seconds
	StartNumber     int
	DiscontAfter    int // segment index after which a discontinuity is set; -1 = none
}

// HLSPlaylist renders an HLS media playlist matching the generated stream.
// Segment URIs are local relative references so the manifest parser's path
// confinement accepts them without any network access.
func HLSPlaylist(s PlaylistSpec, segments int) string {
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n")
	fmt.Fprintf(&b, "#EXT-X-TARGETDURATION:%d\n", int(s.SegmentDuration)+1)
	fmt.Fprintf(&b, "#EXT-X-MEDIA-SEQUENCE:%d\n", s.StartNumber)
	for i := 0; i < segments; i++ {
		if s.DiscontAfter >= 0 && i == s.DiscontAfter+1 {
			b.WriteString("#EXT-X-DISCONTINUITY\n")
		}
		fmt.Fprintf(&b, "#EXTINF:%.3f,\nseg-%d.ts\n", s.SegmentDuration, s.StartNumber+i)
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	return b.String()
}

// DASHPlaylist renders a DASH MPD using SegmentTemplate with $Number$
// expansion and a timescale-denominated fixed segment duration.
func DASHPlaylist(s PlaylistSpec, segments int) string {
	timescale := 1000
	durMS := int(s.SegmentDuration * float64(timescale))
	totalSecs := float64(segments) * s.SegmentDuration
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	fmt.Fprintf(&b, `<MPD type="static" mediaPresentationDuration="PT%.3fS">`+"\n", totalSecs)
	b.WriteString(`  <Period duration="PT` + fmt.Sprintf("%.3f", totalSecs) + `S">` + "\n")
	fmt.Fprintf(&b,
		`    <SegmentTemplate timescale="%d" media="chunk-$Number$.m4s" startNumber="%d" duration="%d"/>`+"\n",
		timescale, s.StartNumber, durMS)
	b.WriteString("  </Period>\n</MPD>\n")
	return b.String()
}

// SegmentDurationSecs derives the manifest segment duration from the stream
// parameters so the playlist and the packet timeline describe the same span.
func SegmentDurationSecs(p Params) float64 {
	spanTicks := uint64(p.PacketsPerSegment) * uint64(p.TSIncrement)
	return float64(spanTicks) / float64(p.ClockRate)
}

// BuildPlaylist renders the requested format (or both) for a stream.
func BuildPlaylist(format manifest.Format, p Params, startNumber, discontAfter int) map[manifest.Format]string {
	spec := PlaylistSpec{
		Format: format, SegmentDuration: SegmentDurationSecs(p),
		StartNumber: startNumber, DiscontAfter: discontAfter,
	}
	out := map[manifest.Format]string{}
	switch format {
	case manifest.FormatHLS:
		out[manifest.FormatHLS] = HLSPlaylist(spec, p.Segments)
	case manifest.FormatDASH:
		out[manifest.FormatDASH] = DASHPlaylist(spec, p.Segments)
	}
	return out
}
