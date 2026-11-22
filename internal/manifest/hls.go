package manifest

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	"mediacore/internal/clock"
)

// ParseHLS parses an HLS media playlist into a Track. EXTINF durations
// are normalized onto the rational clock; negative durations, oversized
// playlists and unsafe URIs are rejected with track/segment positions.
func ParseHLS(r io.Reader, meta TrackMeta) (Track, error) {
	const op = "parse-hls"
	track := Track{TrackMeta: meta}
	lr := io.LimitReader(r, MaxManifestBytes+1)
	sc := bufio.NewScanner(lr)
	sc.Buffer(make([]byte, 64*1024), 1<<20)

	sawHeader := false
	var pending *clock.Tick
	pendingGap := false
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if line == "#EXTM3U" {
			sawHeader = true
			continue
		}
		if strings.HasPrefix(line, "#EXTINF:") {
			rest := strings.TrimPrefix(line, "#EXTINF:")
			if i := strings.IndexByte(rest, ','); i >= 0 {
				rest = rest[:i]
			}
			dur, err := ParseDecimal(rest)
			if err != nil {
				return track, clock.At(clock.CodeManifestInvalid, op, meta.ID,
					len(track.Segments), "line %d: %v", lineNo, err)
			}
			if dur.Sign() < 0 {
				return track, clock.At(clock.CodeNegativeDuration, op, meta.ID,
					len(track.Segments), "line %d: EXTINF %s", lineNo, rest)
			}
			pending = &dur
			continue
		}
		if line == "#EXT-X-GAP" {
			pendingGap = true
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue // other tags are not needed for the unified model
		}
		// Segment URI line.
		if pending == nil {
			return track, clock.At(clock.CodeManifestInvalid, op, meta.ID,
				len(track.Segments), "line %d: URI without EXTINF", lineNo)
		}
		track.Segments = append(track.Segments, Segment{
			Index: len(track.Segments),
			URI:   line,
			Dur:   *pending,
			Gap:   pendingGap,
		})
		if len(track.Segments) > MaxSegments {
			return track, clock.At(clock.CodeManifestTooLarge, op, meta.ID, -1,
				"more than %d segments", MaxSegments)
		}
		pending = nil
		pendingGap = false
	}
	if err := sc.Err(); err != nil {
		return track, clock.Wrap(clock.CodeIO, op, err)
	}
	if !sawHeader {
		return track, clock.At(clock.CodeManifestInvalid, op, meta.ID, -1,
			"missing #EXTM3U header")
	}
	if err := ValidateTrack(track); err != nil {
		return track, err
	}
	return track, nil
}

// FormatHLS renders a track back into a media playlist. It is used by
// the fixture generator so sample data ships with the code.
func FormatHLS(t Track) string {
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:7\n")
	max := 0.0
	for _, s := range t.Segments {
		if d := s.Dur.Seconds(); d > max {
			max = d
		}
	}
	fmt.Fprintf(&b, "#EXT-X-TARGETDURATION:%d\n", int(max)+1)
	for _, s := range t.Segments {
		fmt.Fprintf(&b, "#EXTINF:%s,\n", decimalString(s.Dur))
		if s.Gap {
			b.WriteString("#EXT-X-GAP\n")
		}
		b.WriteString(s.URI + "\n")
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	return b.String()
}

func decimalString(t clock.Tick) string {
	if t.Num%t.Den == 0 {
		return fmt.Sprintf("%d.0", t.Num/t.Den)
	}
	return fmt.Sprintf("%.6f", t.Seconds())
}
