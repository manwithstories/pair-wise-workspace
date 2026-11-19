// Package manifest parses HLS playlists (media and master) and DASH MPDs
// (SegmentTemplate+SegmentTimeline and SegmentList) into a unified segment
// reference list. It rejects escaping paths, non-positive durations and
// inconsistent timescales, and reports per-segment location info.
package manifest

import (
	"fmt"
	"strconv"
	"strings"

	"archive/internal/errs"
)

// Type identifies the manifest flavour.
type Type string

const (
	TypeHLSMedia  Type = "hls-media"
	TypeHLSMaster Type = "hls-master"
	TypeDASH      Type = "dash"
)

// SegmentRef locates one media segment.
type SegmentRef struct {
	Index     int     // sequence number (HLS media-sequence aware)
	URI       string  // relative URI as written in the manifest
	Duration  float64 // seconds, always > 0
	Timescale uint32  // DASH timescale, 0 for HLS
	ByteRange string  // optional "n[@o]" from EXT-X-BYTERANGE
}

// Variant is one rendition listed by a master playlist.
type Variant struct {
	URI       string
	Bandwidth int
	Language  string // from EXT-X-MEDIA, may be empty
	MediaType string // AUDIO/VIDEO/SUBTITLES, empty for stream-inf
}

// Manifest is the parsed result.
type Manifest struct {
	Type           Type
	Segments       []SegmentRef
	Variants       []Variant // master playlists only
	MediaSequence  int
	TargetDuration float64
	Timescale      uint32 // DASH only
}

// Parse detects the manifest format from content and parses it.
func Parse(asset string, data []byte) (*Manifest, error) {
	trimmed := strings.TrimLeft(string(data), " \t\r\n\xef\xbb\xbf")
	switch {
	case strings.HasPrefix(trimmed, "#EXTM3U"):
		return parseHLS(asset, trimmed)
	case strings.Contains(trimmed, "<MPD"):
		return parseDASH(asset, trimmed)
	}
	return nil, errs.New(errs.KindManifest, asset, "header", "unrecognized manifest format", nil)
}

// ValidateURI rejects absolute paths, parent traversal and URL schemes so
// segment references can never escape the asset directory.
func ValidateURI(asset, uri string) error {
	if uri == "" {
		return errs.New(errs.KindManifest, asset, "uri", "empty segment URI", nil)
	}
	if strings.Contains(uri, "://") || strings.HasPrefix(uri, "//") {
		return errs.Newf(errs.KindManifest, asset, "uri", "remote URI rejected: %q", uri)
	}
	if strings.HasPrefix(uri, "/") || strings.HasPrefix(uri, "\\") {
		return errs.Newf(errs.KindManifest, asset, "uri", "absolute path rejected: %q", uri)
	}
	for _, part := range strings.FieldsFunc(uri, func(r rune) bool { return r == '/' || r == '\\' }) {
		if part == ".." {
			return errs.Newf(errs.KindManifest, asset, "uri", "parent traversal rejected: %q", uri)
		}
	}
	return nil
}

func parseHLS(asset, text string) (*Manifest, error) {
	lines := strings.Split(text, "\n")
	m := &Manifest{Type: TypeHLSMedia}
	isMaster := false
	for _, l := range lines {
		if strings.HasPrefix(l, "#EXT-X-STREAM-INF") {
			isMaster = true
			break
		}
	}
	if isMaster {
		m.Type = TypeHLSMaster
	}
	var pendingDur = -1.0
	var pendingRange string
	var pendingInf bool
	seq := m.MediaSequence
	for i, raw := range lines {
		line := strings.TrimSpace(raw)
		loc := fmt.Sprintf("line %d", i+1)
		switch {
		case strings.HasPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"):
			v, err := strconv.Atoi(strings.TrimPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"))
			if err != nil || v < 0 {
				return nil, errs.New(errs.KindManifest, asset, loc, "invalid media sequence", nil)
			}
			m.MediaSequence, seq = v, v
		case strings.HasPrefix(line, "#EXT-X-TARGETDURATION:"):
			v, err := strconv.ParseFloat(strings.TrimPrefix(line, "#EXT-X-TARGETDURATION:"), 64)
			if err != nil || v <= 0 {
				return nil, errs.New(errs.KindManifest, asset, loc, "invalid target duration", nil)
			}
			m.TargetDuration = v
		case strings.HasPrefix(line, "#EXTINF:"):
			v, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimPrefix(line, "#EXTINF:"), ","), 64)
			if err != nil || v <= 0 {
				return nil, errs.Newf(errs.KindManifest, asset, loc, "invalid segment duration %q", line)
			}
			pendingDur, pendingInf = v, true
		case strings.HasPrefix(line, "#EXT-X-BYTERANGE:"):
			pendingRange = strings.TrimPrefix(line, "#EXT-X-BYTERANGE:")
		case strings.HasPrefix(line, "#EXT-X-STREAM-INF:"):
			v := Variant{}
			for _, kv := range strings.Split(strings.TrimPrefix(line, "#EXT-X-STREAM-INF:"), ",") {
				if strings.HasPrefix(kv, "BANDWIDTH=") {
					v.Bandwidth, _ = strconv.Atoi(strings.TrimPrefix(kv, "BANDWIDTH="))
				}
			}
			m.Variants = append(m.Variants, v)
		case strings.HasPrefix(line, "#EXT-X-MEDIA:"):
			v := Variant{MediaType: attr(line, "TYPE"), Language: attr(line, "LANGUAGE"), URI: attr(line, "URI")}
			m.Variants = append(m.Variants, v)
		case line == "" || strings.HasPrefix(line, "#"):
			// comments and other tags
		default:
			if err := ValidateURI(asset, line); err != nil {
				return nil, err
			}
			if m.Type == TypeHLSMaster {
				if len(m.Variants) > 0 && m.Variants[len(m.Variants)-1].URI == "" {
					m.Variants[len(m.Variants)-1].URI = line
				} else {
					m.Variants = append(m.Variants, Variant{URI: line})
				}
				continue
			}
			if !pendingInf {
				return nil, errs.Newf(errs.KindManifest, asset, loc, "segment URI %q without EXTINF", line)
			}
			m.Segments = append(m.Segments, SegmentRef{Index: seq, URI: line, Duration: pendingDur, ByteRange: pendingRange})
			seq++
			pendingDur, pendingRange, pendingInf = -1, "", false
		}
	}
	if m.Type == TypeHLSMedia && len(m.Segments) == 0 {
		return nil, errs.New(errs.KindManifest, asset, "body", "media playlist has no segments", nil)
	}
	return m, nil
}

func attr(line, key string) string {
	for _, kv := range strings.Split(strings.TrimPrefix(line, "#EXT-X-MEDIA:"), ",") {
		if strings.HasPrefix(kv, key+"=") {
			return strings.Trim(strings.TrimPrefix(kv, key+"="), `"`)
		}
	}
	return ""
}
