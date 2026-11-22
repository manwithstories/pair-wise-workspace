// Package manifest parses HLS and DASH playlists into segment tasks and
// merges them into aligned intervals on the unified 90 kHz time base.
package manifest

import (
	"fmt"
	"strconv"
	"strings"

	"mediaroute/internal/clock"
)

// Limits guarding against hostile or corrupt playlists.
const (
	MaxSegments        = 4096
	MaxSegmentDuration = 3600.0 // seconds
	MaxURIlen          = 1024
)

// Segment is one media segment referenced by a playlist.
type Segment struct {
	Seq           int
	URI           string
	DurationTicks uint64 // 90 kHz ticks
}

// Playlist is a parsed HLS media playlist.
type Playlist struct {
	TargetDurationTicks uint64
	Segments            []Segment
}

// ValidateURI rejects path traversal and absolute paths.
func ValidateURI(uri string) error {
	if uri == "" || len(uri) > MaxURIlen {
		return fmt.Errorf("manifest: invalid uri length %d", len(uri))
	}
	if strings.HasPrefix(uri, "/") || strings.HasPrefix(uri, "\\") ||
		strings.Contains(uri, "..") || strings.Contains(uri, ":") {
		return fmt.Errorf("manifest: uri %q escapes base directory", uri)
	}
	return nil
}

// ParseHLS parses a simplified HLS media playlist (#EXTM3U, #EXTINF,
// #EXT-X-TARGETDURATION, segment URIs).
func ParseHLS(data string) (*Playlist, error) {
	if !strings.HasPrefix(strings.TrimSpace(data), "#EXTM3U") {
		return nil, fmt.Errorf("manifest: missing #EXTM3U header")
	}
	p := &Playlist{}
	var pendingDur float64 = -1
	seq := 0
	for n, raw := range strings.Split(data, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		switch {
		case strings.HasPrefix(line, "#EXT-X-TARGETDURATION:"):
			v, err := strconv.ParseFloat(strings.TrimPrefix(line, "#EXT-X-TARGETDURATION:"), 64)
			if err != nil || v <= 0 || v > MaxSegmentDuration {
				return nil, fmt.Errorf("manifest: line %d: bad target duration", n+1)
			}
			p.TargetDurationTicks = clock.MillisToTicks(v * 1000)
		case strings.HasPrefix(line, "#EXTINF:"):
			v, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimPrefix(line, "#EXTINF:"), ","), 64)
			if err != nil || v <= 0 || v > MaxSegmentDuration {
				return nil, fmt.Errorf("manifest: line %d: bad EXTINF duration", n+1)
			}
			pendingDur = v
		case strings.HasPrefix(line, "#"):
			// Other tags ignored.
		default:
			if pendingDur < 0 {
				return nil, fmt.Errorf("manifest: line %d: segment without EXTINF", n+1)
			}
			if err := ValidateURI(line); err != nil {
				return nil, err
			}
			if seq >= MaxSegments {
				return nil, fmt.Errorf("manifest: exceeds %d segments", MaxSegments)
			}
			p.Segments = append(p.Segments, Segment{
				Seq: seq, URI: line, DurationTicks: clock.MillisToTicks(pendingDur * 1000),
			})
			seq++
			pendingDur = -1
		}
	}
	if len(p.Segments) == 0 {
		return nil, fmt.Errorf("manifest: playlist has no segments")
	}
	return p, nil
}
