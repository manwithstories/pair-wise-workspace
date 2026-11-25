package manifest

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// ParseHLSFile parses a local media playlist.
func ParseHLSFile(path string) (*Manifest, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("manifest: open hls %s: %w", path, err)
	}
	defer f.Close()
	m, err := ParseHLS(f)
	if err != nil {
		return nil, err
	}
	m.SourcePath = path
	return m, nil
}

// ParseHLS parses an HLS media playlist from a reader. It understands
// the subset used by the archive fixtures: EXTM3U, VERSION,
// MEDIA-SEQUENCE, DISCONTINUITY, MAP and EXTINF plus segment URIs.
func ParseHLS(r io.Reader) (*Manifest, error) {
	m := &Manifest{Kind: KindHLS}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var currentMap string
	mapAnchored := map[string]bool{}
	pendingDiscontinuity := false
	var pendingDuration float64

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}

		if strings.HasPrefix(line, "#EXT-X-MEDIA-SEQUENCE:") {
			v, err := strconv.ParseInt(strings.TrimPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"), 10, 64)
			if err != nil {
				return nil, fmt.Errorf("manifest: bad media sequence: %w", err)
			}
			m.MediaSequence = v
			m.Anchors = append(m.Anchors, Anchor{Kind: AnchorSequence, SegmentIndex: 0, Ticks: v})
			continue
		}
		if line == "#EXT-X-DISCONTINUITY" {
			pendingDiscontinuity = true
			continue
		}
		if strings.HasPrefix(line, "#EXT-X-MAP:") {
			uri := parseAttr(line[len("#EXT-X-MAP:"):], "URI")
			if err := CheckRef(uri); err != nil {
				return nil, err
			}
			currentMap = uri
			continue
		}
		if strings.HasPrefix(line, "#EXTINF:") {
			body := line[len("#EXTINF:"):]
			if i := strings.IndexByte(body, ','); i >= 0 {
				body = body[:i]
			}
			d, err := strconv.ParseFloat(strings.TrimSpace(body), 64)
			if err != nil {
				return nil, fmt.Errorf("manifest: bad EXTINF %q: %w", line, err)
			}
			pendingDuration = d
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue // VERSION and other tags do not affect the model
		}

		// Segment URI line.
		if err := CheckRef(line); err != nil {
			return nil, err
		}
		idx := len(m.Segments)
		seg := Segment{
			Index:         idx,
			Sequence:      m.MediaSequence + int64(idx),
			URI:           line,
			MapURI:        currentMap,
			DurationSec:   pendingDuration,
			Discontinuity: pendingDiscontinuity,
		}
		m.Segments = append(m.Segments, seg)

		if pendingDiscontinuity {
			m.Anchors = append(m.Anchors, Anchor{
				Kind: AnchorDiscontinuity, SegmentIndex: idx,
				Detail: "declared by EXT-X-DISCONTINUITY",
			})
			pendingDiscontinuity = false
		}
		if currentMap != "" && !mapAnchored[currentMap] {
			mapAnchored[currentMap] = true
			m.Anchors = append(m.Anchors, Anchor{
				Kind: AnchorMap, SegmentIndex: idx, Detail: currentMap,
			})
		}
		pendingDuration = 0
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("manifest: read hls: %w", err)
	}
	if len(m.Segments) == 0 {
		return nil, fmt.Errorf("manifest: hls playlist contains no segments")
	}
	return m, nil
}

// parseAttr extracts ATTR=value or ATTR="value" from a tag attribute
// list without pulling in a full HLS attribute grammar.
func parseAttr(attrs, key string) string {
	for _, part := range splitAttrs(attrs) {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 || strings.TrimSpace(kv[0]) != key {
			continue
		}
		v := strings.TrimSpace(kv[1])
		return strings.Trim(v, `"`)
	}
	return ""
}

func splitAttrs(s string) []string {
	var out []string
	var b strings.Builder
	inQuote := false
	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
			b.WriteRune(r)
		case r == ',' && !inQuote:
			out = append(out, b.String())
			b.Reset()
		default:
			b.WriteRune(r)
		}
	}
	if b.Len() > 0 {
		out = append(out, b.String())
	}
	return out
}
