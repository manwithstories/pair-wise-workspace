package fixture

import (
	"fmt"
	"strings"

	"mediad/internal/clock"
)

// buildHLS renders a media playlist referencing the generated segments and
// the shared init segment.
func buildHLS(b *Batch) ([]byte, error) {
	base := clock.Default()
	var s strings.Builder
	s.WriteString("#EXTM3U\n#EXT-X-VERSION:7\n")
	s.WriteString(fmt.Sprintf("#EXT-X-TARGETDURATION:%d\n", b.Segments[0].Duration/base.TicksFromSeconds(1)))
	s.WriteString("#EXT-X-MEDIA-SEQUENCE:1\n")
	s.WriteString("#EXT-X-PLAYLIST-TYPE:VOD\n")
	s.WriteString(fmt.Sprintf("#EXT-X-MAP:URI=%q\n", InitName))
	for _, seg := range b.Segments {
		s.WriteString(fmt.Sprintf("#EXTINF:%s,\n", clock.FormatDecimalTicks(seg.Duration, base.Timescale, 3)))
		s.WriteString(seg.URI)
		s.WriteString("\n")
	}
	s.WriteString("#EXT-X-ENDLIST\n")
	return []byte(s.String()), nil
}

// buildDASH renders an MPD whose SegmentTimeline mirrors the HLS durations.
// The generated segments use a 48 kHz media timescale.
func buildDASH(b *Batch, opts Options) ([]byte, error) {
	const mediaTimescale = 48000
	segNative := b.Segments[0].Duration * int64(mediaTimescale) / int64(clock.DefaultTimescale)
	total := int64(len(b.Segments)) * segNative
	var s strings.Builder
	s.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	s.WriteString(`<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" profiles="urn:mpeg:dash:profile:isoff-live:2011"`)
	s.WriteString(fmt.Sprintf(` mediaPresentationDuration="%s" minBufferTime="PT2S">`+"\n",
		clock.FormatISODurationTicks(total, mediaTimescale)))
	s.WriteString(`  <Period id="p0" start="PT0S">` + "\n")
	s.WriteString(`    <AdaptationSet mimeType="audio/mp4" codecs="mp4a.40.2">` + "\n")
	s.WriteString(fmt.Sprintf(`      <SegmentTemplate timescale="%d" initialization="%s" media="seg$Number%%03d$.m4s" startNumber="0" presentationTimeOffset="0">`+"\n",
		mediaTimescale, InitName))
	s.WriteString("        <SegmentTimeline>\n")
	s.WriteString(fmt.Sprintf(`          <S t="0" d="%d" r="%d"/>`+"\n", segNative, len(b.Segments)-1))
	s.WriteString("        </SegmentTimeline>\n")
	s.WriteString("      </SegmentTemplate>\n")
	s.WriteString(`      <Representation id="seg" bandwidth="128000"/>` + "\n")
	s.WriteString("    </AdaptationSet>\n  </Period>\n</MPD>\n")
	return []byte(s.String()), nil
}
