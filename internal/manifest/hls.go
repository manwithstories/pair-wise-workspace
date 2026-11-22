package manifest

import (
	"bufio"
	"bytes"
	"strings"

	"archive/internal/clock"
	"archive/internal/fault"
)

// ParseHLS parses an HLS media playlist into ordered segment
// descriptors. It rejects negative durations, duration overflow and
// URIs escaping the manifest base. Duplicate URIs are preserved; the
// pipeline deduplicates them as a recoverable anomaly.
func ParseHLS(data []byte, source, base string) ([]Segment, error) {
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	first := true
	var segs []Segment
	var pending *clock.Rational
	start := clock.Must(0, 1)
	started := false
	lineNo := 0
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		lineNo++
		if lineNo == 1 {
			if line != "#EXTM3U" {
				return nil, fault.Newf(fault.Fatal, "hls", source, "missing #EXTM3U header")
			}
			first = false
			continue
		}
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#EXT-X-ARCHIVE-BASE:") {
			if started {
				return nil, fault.Newf(fault.Fatal, "hls", source,
					"archive base after segments (line %d)", lineNo)
			}
			b, err := parseDecimal(strings.TrimPrefix(line, "#EXT-X-ARCHIVE-BASE:"))
			if err != nil {
				return nil, err
			}
			start = b
			continue
		}
		if strings.HasPrefix(line, "#EXTINF:") {
			d, err := parseDecimal(strings.TrimSuffix(strings.TrimPrefix(line, "#EXTINF:"), ","))
			if err != nil {
				return nil, err
			}
			pending = &d
			continue
		}
		if strings.HasPrefix(line, "#") {
			if err := checkHLSTag(line, source); err != nil {
				return nil, err
			}
			continue
		}
		// URI line
		if pending == nil {
			return nil, fault.Newf(fault.Fatal, "hls", source, "segment URI without #EXTINF (line %d)", lineNo)
		}
		uri, err := SafeJoin(base, line)
		if err != nil {
			return nil, err
		}
		segs = append(segs, Segment{
			Index:  len(segs),
			URI:    uri,
			Start:  start,
			Dur:    *pending,
			Source: source,
		})
		started = true
		start, err = start.Add(*pending)
		if err != nil {
			return nil, fault.Wrap(fault.Fatal, "hls", source, err)
		}
		pending = nil
	}
	if err := sc.Err(); err != nil {
		return nil, fault.Wrap(fault.Fatal, "hls", source, err)
	}
	if first {
		return nil, fault.Newf(fault.Fatal, "hls", source, "empty playlist")
	}
	if len(segs) == 0 {
		return nil, fault.Newf(fault.Fatal, "hls", source, "playlist declares no segments")
	}
	return segs, nil
}

// checkHLSTag validates the tags we understand and rejects malformed
// numeric fields that could overflow later arithmetic.
func checkHLSTag(line, source string) error {
	switch {
	case strings.HasPrefix(line, "#EXT-X-TARGETDURATION:"):
		d, err := parseDecimal(strings.TrimPrefix(line, "#EXT-X-TARGETDURATION:"))
		if err != nil {
			return err
		}
		if d.Cmp(clock.Must(maxDurationSecs, 1)) > 0 {
			return fault.Newf(fault.Fatal, "hls", source, "target duration overflow")
		}
	case strings.HasPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"),
		strings.HasPrefix(line, "#EXT-X-VERSION:"),
		strings.HasPrefix(line, "#EXT-X-PLAYLIST-TYPE:"),
		strings.HasPrefix(line, "#EXT-X-ENDLIST"),
		strings.HasPrefix(line, "#EXT-X-MAP:"),
		strings.HasPrefix(line, "#EXT-X-INDEPENDENT-SEGMENTS"):
		// accepted, no numeric hazard
	default:
		// Unknown tags are ignored; they carry no timeline data.
	}
	return nil
}
